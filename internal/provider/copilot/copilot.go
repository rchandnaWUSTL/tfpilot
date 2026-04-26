package copilot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/openai"
)

// Provider wraps the OpenAI chat provider with Copilot-specific auth:
//   - long-lived GitHub token (from device flow, cached on disk)
//   - short-lived Copilot chat token (exchanged on demand from GitHub token)
//   - 401-triggered re-exchange wired through openai.Provider.RefreshFn
//
// The outer Provider handles all credential state; the inner openai.Provider
// sees a plain Bearer token and has no idea it's talking to Copilot.
type Provider struct {
	store      Store
	deviceFlow DeviceFlowFn
	httpClient *http.Client
	promptIn   io.Reader
	promptOut  io.Writer

	apiBaseOverride  string
	chatBaseOverride string

	mu         sync.Mutex
	cached     *CachedToken
	copilotTok *CopilotToken
	inner      *openai.Provider
}

// Options configures a Copilot Provider. All fields are optional; zero values
// yield the production defaults (file-backed store, real device flow, stdin
// prompt).
type Options struct {
	Store       Store
	DeviceFlow  DeviceFlowFn
	HTTPClient  *http.Client
	PromptIn    io.Reader
	PromptOut   io.Writer
	// APIBaseOverride / ChatBaseOverride are test seams that bypass the
	// deployment-type routing so an httptest server can stand in for the real
	// GitHub and Copilot hosts.
	APIBaseOverride  string
	ChatBaseOverride string
}

func New(opts Options) *Provider {
	p := &Provider{
		store:            opts.Store,
		deviceFlow:       opts.DeviceFlow,
		httpClient:       opts.HTTPClient,
		promptIn:         opts.PromptIn,
		promptOut:        opts.PromptOut,
		apiBaseOverride:  opts.APIBaseOverride,
		chatBaseOverride: opts.ChatBaseOverride,
	}
	if p.store == nil {
		path, err := DefaultStorePath()
		if err == nil {
			p.store = NewFileStore(path)
		}
	}
	if p.deviceFlow == nil {
		p.deviceFlow = RunDeviceFlow
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if p.promptIn == nil {
		p.promptIn = os.Stdin
	}
	if p.promptOut == nil {
		p.promptOut = os.Stdout
	}
	return p
}

func (p *Provider) Name() string { return "copilot" }

// Authenticate loads (or mints) a GitHub token, exchanges it for a Copilot
// chat token, and constructs the inner OpenAI provider wired with the refresh
// hook.
func (p *Provider) Authenticate(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.store == nil {
		return fmt.Errorf("copilot: no token store configured")
	}

	cached, err := p.store.Load()
	if err != nil {
		return fmt.Errorf("load copilot cache: %w", err)
	}
	if cached == nil {
		deployment, domain, err := promptDeployment(p.promptIn, p.promptOut)
		if err != nil {
			return fmt.Errorf("copilot deployment prompt: %w", err)
		}
		stub := CachedToken{DeploymentType: deployment, EnterpriseDomain: domain}
		ghToken, err := p.deviceFlow(ctx, authDomain(stub), p.promptOut)
		if err != nil {
			return fmt.Errorf("copilot device flow: %w", err)
		}
		cached = &CachedToken{
			GitHubToken:      ghToken,
			DeploymentType:   deployment,
			EnterpriseDomain: domain,
		}
		if err := p.store.Save(cached); err != nil {
			return fmt.Errorf("save copilot cache: %w", err)
		}
	}
	p.cached = cached

	tok, err := p.exchange(ctx)
	if err != nil {
		return err
	}
	p.copilotTok = tok

	p.inner = openai.New(openai.Options{
		Name:         "copilot",
		BaseURL:      p.chatBase(),
		APIKey:       tok.Token,
		HTTPClient:   p.httpClient,
		ExtraHeaders: copilotHeaders(),
		RefreshFn:    p.refreshHook,
	})
	return nil
}

// SendMessage delegates to the inner OpenAI provider. Authenticate must be
// called first.
func (p *Provider) SendMessage(ctx context.Context, req provider.SendRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	inner := p.inner
	p.mu.Unlock()
	if inner == nil {
		return nil, fmt.Errorf("copilot: Authenticate must be called before SendMessage")
	}
	return inner.SendMessage(ctx, req)
}

// refreshHook is handed to the inner OpenAI provider. On HTTP 401 from
// Copilot's chat endpoint, re-exchange the GitHub token for a fresh Copilot
// token, push it into the inner provider, and return the new Authorization
// header so the openai.Provider can retry exactly once.
func (p *Provider) refreshHook(ctx context.Context) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tok, err := p.exchange(ctx)
	if err != nil {
		return "", false, err
	}
	p.copilotTok = tok
	if p.inner != nil {
		p.inner.SetAPIKey(tok.Token)
	}
	return "Bearer " + tok.Token, true, nil
}

func (p *Provider) exchange(ctx context.Context) (*CopilotToken, error) {
	if p.cached == nil {
		return nil, fmt.Errorf("copilot: no github token loaded")
	}
	return ExchangeCopilotToken(ctx, p.httpClient, p.apiBase(), p.cached.GitHubToken)
}

func (p *Provider) apiBase() string {
	if p.apiBaseOverride != "" {
		return p.apiBaseOverride
	}
	return apiBaseURL(*p.cached)
}

func (p *Provider) chatBase() string {
	if p.chatBaseOverride != "" {
		return p.chatBaseOverride
	}
	// Prefer the URL from the token exchange response — for Business/Enterprise
	// this routes to api.business.githubcopilot.com etc. Fall back to the
	// deployment-type static routing only if the server didn't supply one.
	if p.copilotTok != nil && p.copilotTok.APIEndpoint != "" {
		return p.copilotTok.APIEndpoint
	}
	return chatBaseURL(*p.cached)
}
