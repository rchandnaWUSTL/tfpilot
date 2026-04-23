package copilot_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/copilot"
)

// inMemStore is a test-only Store that keeps a single CachedToken in memory.
type inMemStore struct {
	tok   *copilot.CachedToken
	saves int32
}

func (s *inMemStore) Load() (*copilot.CachedToken, error) { return s.tok, nil }
func (s *inMemStore) Save(t *copilot.CachedToken) error {
	atomic.AddInt32(&s.saves, 1)
	s.tok = t
	return nil
}

// TestCopilot_401RefreshRetry verifies the full Copilot auth loop:
//  1. Authenticate with a cached GitHub token triggers a Copilot-token exchange.
//  2. First chat call fails with 401 (simulating an expired Copilot token).
//  3. The openai.Provider's RefreshFn invokes our refresh hook, which re-runs
//     the exchange and produces a NEW Copilot token.
//  4. The retried chat call succeeds carrying the new Bearer token.
//
// This is the single most important Copilot behavior that opencode explicitly
// does NOT implement — if this test breaks, a user mid-session sees a hard
// failure instead of a transparent refresh.
func TestCopilot_401RefreshRetry(t *testing.T) {
	const githubToken = "gh_test_long_lived"

	var exchangeCalls int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token "+githubToken {
			t.Errorf("exchange: wrong Authorization header: got %q", got)
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		n := atomic.AddInt32(&exchangeCalls, 1)
		fmt.Fprintf(w, `{"token":"copilot_tok_v%d","expires_at":0}`, n)
	}))
	defer apiSrv.Close()

	var chatCalls int32
	var authHeaders []string
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if v := r.Header.Get("Copilot-Integration-Id"); v == "" {
			t.Errorf("missing Copilot-Integration-Id header")
		}
		if v := r.Header.Get("Editor-Version"); v == "" {
			t.Errorf("missing Editor-Version header")
		}
		n := atomic.AddInt32(&chatCalls, 1)
		if n == 1 {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer chatSrv.Close()

	store := &inMemStore{tok: &copilot.CachedToken{
		GitHubToken:    githubToken,
		DeploymentType: copilot.DeploymentIndividual,
	}}

	prov := copilot.New(copilot.Options{
		Store:            store,
		APIBaseOverride:  apiSrv.URL,
		ChatBaseOverride: chatSrv.URL,
		DeviceFlow: func(ctx context.Context, domain string, out io.Writer) (string, error) {
			t.Fatalf("device flow should not be invoked when cache is populated")
			return "", nil
		},
		PromptIn:  strings.NewReader(""),
		PromptOut: &bytes.Buffer{},
	})

	if err := prov.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := atomic.LoadInt32(&exchangeCalls); got != 1 {
		t.Fatalf("expected 1 exchange call after Authenticate, got %d", got)
	}

	ch, err := prov.SendMessage(context.Background(), provider.SendRequest{
		Model: "gpt-4o",
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "ping"}},
		}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var gotText string
	var stop provider.StopReason
	for ev := range ch {
		switch ev.Type {
		case provider.EventText:
			gotText += ev.TextDelta
		case provider.EventStop:
			stop = ev.StopReason
		case provider.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if gotText != "ok" {
		t.Errorf("text: got %q want %q", gotText, "ok")
	}
	if stop != provider.StopEndTurn {
		t.Errorf("stop reason: got %q want %q", stop, provider.StopEndTurn)
	}
	if got := atomic.LoadInt32(&chatCalls); got != 2 {
		t.Errorf("expected 2 chat calls (401 + retry), got %d", got)
	}
	if got := atomic.LoadInt32(&exchangeCalls); got != 2 {
		t.Errorf("expected 2 exchange calls (initial + post-401), got %d", got)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 auth headers recorded, got %d", len(authHeaders))
	}
	if authHeaders[0] == authHeaders[1] {
		t.Errorf("retry did not rotate bearer: both calls used %q", authHeaders[0])
	}
	if !strings.Contains(authHeaders[0], "copilot_tok_v1") {
		t.Errorf("first call used wrong token: %q", authHeaders[0])
	}
	if !strings.Contains(authHeaders[1], "copilot_tok_v2") {
		t.Errorf("retry used wrong token: %q", authHeaders[1])
	}
}

// TestCopilot_DeviceFlowOnEmptyCache verifies that when no GitHub token is
// cached, Authenticate drives the injected DeviceFlowFn, persists the result,
// and then exchanges it for a Copilot token.
func TestCopilot_DeviceFlowOnEmptyCache(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"copilot_fresh","expires_at":0}`)
	}))
	defer apiSrv.Close()

	store := &inMemStore{}

	var flowCalls int32
	prov := copilot.New(copilot.Options{
		Store:           store,
		APIBaseOverride: apiSrv.URL,
		DeviceFlow: func(ctx context.Context, domain string, out io.Writer) (string, error) {
			atomic.AddInt32(&flowCalls, 1)
			if domain != "github.com" {
				t.Errorf("individual deployment should use github.com, got %q", domain)
			}
			return "gh_from_device_flow", nil
		},
		PromptIn:  strings.NewReader("1\n"), // individual
		PromptOut: &bytes.Buffer{},
	})

	if err := prov.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := atomic.LoadInt32(&flowCalls); got != 1 {
		t.Errorf("expected 1 device flow invocation, got %d", got)
	}
	if store.tok == nil || store.tok.GitHubToken != "gh_from_device_flow" {
		t.Errorf("token not saved: %+v", store.tok)
	}
	if store.tok.DeploymentType != copilot.DeploymentIndividual {
		t.Errorf("deployment type not saved: %q", store.tok.DeploymentType)
	}
	if atomic.LoadInt32(&store.saves) != 1 {
		t.Errorf("expected exactly 1 save, got %d", atomic.LoadInt32(&store.saves))
	}
}
