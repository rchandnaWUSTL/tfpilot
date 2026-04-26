package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClientID is the GitHub App that GitHub allow-lists for the Copilot token
// exchange endpoint (api.github.com/copilot_internal/v2/token). This is the
// VS Code Copilot Chat extension's GitHub App ID — every open-source Copilot
// client we surveyed (CopilotChat.nvim, ericc-ch/copilot-api, etc.) uses the
// same value. Custom GitHub Apps are not accepted by the token exchange
// endpoint; using a different client_id produces a 404.
const ClientID = "Iv1.b507a08c87ecfe98"

// Version is reported in the Copilot-required User-Agent / Editor-Version
// headers. Keep in lockstep with the binary's release tag.
const Version = "tfpilot/0.2.0"

// DeploymentType distinguishes github.com accounts (individual Copilot) from
// customers on GitHub Enterprise Cloud / Server. The device-flow and chat
// endpoints share a single code path — only the hostnames differ.
type DeploymentType string

const (
	DeploymentIndividual DeploymentType = "individual"
	DeploymentEnterprise DeploymentType = "enterprise"
)

// CachedToken is what we persist to disk. The GitHub token is long-lived (until
// the user revokes the grant); the short-lived Copilot chat token is kept only
// in memory and re-minted on every 401.
type CachedToken struct {
	GitHubToken      string         `json:"github_token"`
	DeploymentType   DeploymentType `json:"deployment_type"`
	EnterpriseDomain string         `json:"enterprise_domain,omitempty"`
}

// CopilotToken is the in-memory short-lived chat bearer minted from the GitHub
// token. APIEndpoint is the base URL returned by the exchange under
// `endpoints.api` — for Copilot Business/Enterprise it points at
// api.business.githubcopilot.com or api.enterprise.githubcopilot.com, which is
// why we must honor it rather than hardcode api.githubcopilot.com.
type CopilotToken struct {
	Token       string
	ExpiresAt   time.Time
	APIEndpoint string
}

// Store abstracts persistence so tests can inject an in-memory implementation.
type Store interface {
	Load() (*CachedToken, error)
	Save(*CachedToken) error
}

// DefaultStorePath returns ~/.tfpilot/copilot.json.
func DefaultStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tfpilot", "copilot.json"), nil
}

// NewFileStore returns a Store backed by a 0600-permission file.
func NewFileStore(path string) Store { return &fileStore{path: path} }

type fileStore struct{ path string }

func (f *fileStore) Load() (*CachedToken, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var t CachedToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	if t.GitHubToken == "" {
		return nil, nil
	}
	return &t, nil
}

func (f *fileStore) Save(t *CachedToken) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0o600)
}

// authDomain returns the hostname used for GitHub OAuth endpoints. For
// enterprise, this is the customer's GHE domain; for individual, github.com.
func authDomain(t CachedToken) string {
	if t.DeploymentType == DeploymentEnterprise && t.EnterpriseDomain != "" {
		return t.EnterpriseDomain
	}
	return "github.com"
}

// apiBaseURL returns the REST API base used for the Copilot-token exchange.
func apiBaseURL(t CachedToken) string {
	if t.DeploymentType == DeploymentEnterprise && t.EnterpriseDomain != "" {
		return fmt.Sprintf("https://api.%s", t.EnterpriseDomain)
	}
	return "https://api.github.com"
}

// chatBaseURL returns the Copilot chat completions base.
func chatBaseURL(t CachedToken) string {
	if t.DeploymentType == DeploymentEnterprise && t.EnterpriseDomain != "" {
		return fmt.Sprintf("https://copilot-api.%s", t.EnterpriseDomain)
	}
	return "https://api.githubcopilot.com"
}

// copilotHeaders are the fixed headers that Copilot requires on every chat
// request beyond Authorization. Omitting any of them yields 400 or 401.
func copilotHeaders() map[string]string {
	return map[string]string{
		"Editor-Version":         Version,
		"Editor-Plugin-Version":  Version,
		"User-Agent":             Version,
		"Copilot-Integration-Id": "vscode-chat",
	}
}

// DeviceFlowFn runs the OAuth device authorization grant against the supplied
// GitHub domain and returns a long-lived GitHub access token. Injected so tests
// can skip real network calls.
type DeviceFlowFn func(ctx context.Context, domain string, out io.Writer) (githubToken string, err error)

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type deviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error,omitempty"`
	ErrorDesc   string `json:"error_description,omitempty"`
}

// RunDeviceFlow performs RFC 8628 device authorization against a GitHub-style
// endpoint. Prints the verification URI and user code to `out` and polls until
// the user completes the grant, the code expires, or ctx is canceled.
func RunDeviceFlow(ctx context.Context, domain string, out io.Writer) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	codeURL := fmt.Sprintf("https://%s/login/device/code", domain)
	// Scope matches every working OSS Copilot client (opencode, CopilotChat.nvim,
	// copilot-api). The Copilot token-exchange endpoint authorizes on GitHub App
	// identity, not scope, so read:user is sufficient — broader scopes gain
	// nothing and expand the permissions the user must grant.
	codeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codeURL,
		strings.NewReader(url.Values{
			"client_id": {ClientID},
			"scope":     {"read:user"},
		}.Encode()))
	if err != nil {
		return "", err
	}
	codeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	codeReq.Header.Set("Accept", "application/json")
	codeReq.Header.Set("User-Agent", Version)
	codeResp, err := client.Do(codeReq)
	if err != nil {
		return "", fmt.Errorf("device code request: %w", err)
	}
	codeRaw, _ := io.ReadAll(codeResp.Body)
	codeResp.Body.Close()
	if codeResp.StatusCode >= 400 {
		return "", fmt.Errorf("device code request failed: %s: %s", codeResp.Status, string(codeRaw))
	}
	var dc deviceCodeResponse
	if err := json.Unmarshal(codeRaw, &dc); err != nil {
		return "", fmt.Errorf("parse device code response: %w (body=%q)", err, string(codeRaw))
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Open %s in your browser and enter the code:\n", dc.VerificationURI)
	fmt.Fprintf(out, "    %s\n", dc.UserCode)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Waiting for authorization...")

	tokenURL := fmt.Sprintf("https://%s/login/oauth/access_token", domain)
	interval := time.Duration(dc.Interval+3) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("device code expired before authorization")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		formBody := url.Values{
			"client_id":   {ClientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}.Encode()
		tokReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(formBody))
		if err != nil {
			return "", err
		}
		tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokReq.Header.Set("Accept", "application/json")
		tokReq.Header.Set("User-Agent", Version)
		tokResp, err := client.Do(tokReq)
		if err != nil {
			return "", fmt.Errorf("token poll: %w", err)
		}
		rawBody, _ := io.ReadAll(tokResp.Body)
		tokResp.Body.Close()
		if tokResp.StatusCode >= 400 && len(rawBody) > 0 && rawBody[0] != '{' {
			return "", fmt.Errorf("token poll HTTP %s: non-JSON body: %s", tokResp.Status, string(rawBody))
		}
		var tr deviceTokenResponse
		if decErr := json.Unmarshal(rawBody, &tr); decErr != nil {
			return "", fmt.Errorf("parse token response: %w (body=%q)", decErr, string(rawBody))
		}

		if tr.AccessToken != "" {
			return tr.AccessToken, nil
		}
		switch tr.Error {
		case "authorization_pending":
			// keep polling at current interval
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return "", fmt.Errorf("device code expired")
		case "access_denied":
			return "", fmt.Errorf("authorization denied")
		default:
			return "", fmt.Errorf("token poll error: %s: %s", tr.Error, tr.ErrorDesc)
		}
	}
}

// isTimeoutError detects if an error is a network timeout.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ExchangeCopilotToken trades a long-lived GitHub token for a short-lived
// Copilot chat bearer. The endpoint returns JSON including `token`,
// `expires_at` (unix seconds), and `endpoints.api` (the per-account chat base
// URL — distinct for Business / Enterprise).
func ExchangeCopilotToken(ctx context.Context, httpClient *http.Client, apiBase, githubToken string) (*CopilotToken, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	} else if httpClient.Timeout == 0 {
		httpClient = &http.Client{Transport: httpClient.Transport, Timeout: 30 * time.Second}
	}
	exchangeURL := apiBase + "/copilot_internal/v2/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exchangeURL, nil)
	if err != nil {
		return nil, err
	}
	// Match the VSCode Copilot Chat extension headers — GitHub's Copilot token
	// endpoint checks the GitHub App that minted the token, but also inspects
	// Editor-Version / User-Agent when deciding which Copilot tier applies.
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("Editor-Version", "vscode/1.99.3")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	resp, err := httpClient.Do(req)
	if err != nil {
		if isTimeoutError(err) {
			return nil, fmt.Errorf("Copilot token exchange timed out. Check your network connection and try again.")
		}
		return nil, fmt.Errorf("copilot token exchange: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("copilot token exchange failed: %s: %s", resp.Status, string(raw))
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("parse copilot token response: %w (body=%q)", err, string(raw))
	}
	if body.Token == "" {
		return nil, fmt.Errorf("copilot token exchange: empty token (body=%q)", string(raw))
	}
	exp := time.Unix(body.ExpiresAt, 0)
	if body.ExpiresAt == 0 {
		exp = time.Now().Add(25 * time.Minute)
	}
	return &CopilotToken{
		Token:       body.Token,
		ExpiresAt:   exp,
		APIEndpoint: strings.TrimRight(body.Endpoints.API, "/"),
	}, nil
}

// promptDeployment asks the user whether they're on github.com or enterprise.
// Enterprise responders supply their GHE domain. Reads a single line from `in`
// and writes prompts to `out`.
func promptDeployment(in io.Reader, out io.Writer) (DeploymentType, string, error) {
	reader := bufio.NewReader(in)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Copilot deployment type:")
	fmt.Fprintln(out, "    1) github.com (individual or org Copilot)")
	fmt.Fprintln(out, "    2) GitHub Enterprise (self-hosted or Cloud with custom domain)")
	fmt.Fprint(out, "  Choice [1]: ")
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "1" {
		return DeploymentIndividual, "", nil
	}
	if line != "2" {
		return "", "", fmt.Errorf("invalid choice %q", line)
	}
	fmt.Fprint(out, "  GHE domain (e.g. github.mycompany.com): ")
	domain, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", "", fmt.Errorf("enterprise domain is required")
	}
	return DeploymentEnterprise, domain, nil
}
