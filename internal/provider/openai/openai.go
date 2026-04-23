package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
)

// Provider speaks the OpenAI Chat Completions API over HTTP/SSE. It accepts an
// arbitrary BaseURL and auth mechanism so the same implementation services both
// OpenAI proper and Copilot (which proxies an OpenAI-compatible endpoint).
type Provider struct {
	baseURL      string
	apiKey       string
	name         string
	httpClient   *http.Client
	extraHeaders map[string]string
	authFn       AuthFunc
	refreshFn    RefreshFunc
}

// AuthFunc is called once at startup. It must populate any state the provider
// needs to make authenticated requests (most often by calling back into the
// wrapping provider to set headers / tokens). Return an error to abort startup.
type AuthFunc func(ctx context.Context) error

// RefreshFunc is invoked on HTTP 401. Return true plus an updated Authorization
// value if the provider should retry the call exactly once; return false to
// propagate the 401 as an error.
type RefreshFunc func(ctx context.Context) (authHeader string, ok bool, err error)

type Options struct {
	Name         string // "openai" or "copilot"
	BaseURL      string // e.g. "https://api.openai.com/v1" or "https://api.githubcopilot.com"
	APIKey       string // Bearer token (may be updated via RefreshFunc)
	HTTPClient   *http.Client
	ExtraHeaders map[string]string
	AuthFn       AuthFunc
	RefreshFn    RefreshFunc
}

func New(opts Options) *Provider {
	name := opts.Name
	if name == "" {
		name = "openai"
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	apiKey := opts.APIKey
	if apiKey == "" && name == "openai" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Provider{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		name:         name,
		httpClient:   hc,
		extraHeaders: opts.ExtraHeaders,
		authFn:       opts.AuthFn,
		refreshFn:    opts.RefreshFn,
	}
}

// SetAPIKey is used by Copilot's refresh logic to rotate the bearer token.
func (p *Provider) SetAPIKey(k string) { p.apiKey = k }

func (p *Provider) Name() string { return p.name }

func (p *Provider) Authenticate(ctx context.Context) error {
	if p.authFn != nil {
		return p.authFn(ctx)
	}
	if p.apiKey == "" {
		return fmt.Errorf("✗ OPENAI_API_KEY not found in environment")
	}
	return nil
}

// Wire types for the OpenAI chat completions API.
type chatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"` // string for text-only; nil on tool-call-only assistant messages
	ToolCalls  []toolCall       `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type toolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function toolCallFunc    `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Tools     []chatTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatToolFn  `json:"function"`
}

type chatToolFn struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
}

type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Content   string           `json:"content"`
	ToolCalls []streamToolCall `json:"tool_calls"`
}

type streamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function streamToolCallFunc `json:"function"`
}

type streamToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (p *Provider) SendMessage(ctx context.Context, req provider.SendRequest) (<-chan provider.StreamEvent, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, authHeader, err := p.doChat(ctx, body)
	if err != nil {
		return nil, err
	}

	ch := make(chan provider.StreamEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		p.consumeStream(resp.Body, ch)
		_ = authHeader
	}()
	return ch, nil
}

// doChat performs the HTTP POST with Bearer auth, retrying once on 401 if a
// RefreshFunc is configured. Returns the (still open) response body on success.
func (p *Provider) doChat(ctx context.Context, body []byte) (*http.Response, string, error) {
	resp, authHeader, err := p.postChat(ctx, body, fmt.Sprintf("Bearer %s", p.apiKey))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusUnauthorized && p.refreshFn != nil {
		resp.Body.Close()
		newAuth, ok, refreshErr := p.refreshFn(ctx)
		if refreshErr != nil {
			return nil, "", fmt.Errorf("token refresh: %w", refreshErr)
		}
		if !ok {
			return nil, "", fmt.Errorf("unauthorized (401) and refresh declined")
		}
		resp2, authHeader2, err := p.postChat(ctx, body, newAuth)
		if err != nil {
			return nil, "", err
		}
		if resp2.StatusCode >= 400 {
			defer resp2.Body.Close()
			msg, _ := io.ReadAll(resp2.Body)
			return nil, "", fmt.Errorf("%s chat request failed after refresh: %s: %s", p.name, resp2.Status, string(msg))
		}
		return resp2, authHeader2, nil
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("%s chat request failed: %s: %s", p.name, resp.Status, string(msg))
	}
	return resp, authHeader, nil
}

func (p *Provider) postChat(ctx context.Context, body []byte, authHeader string) (*http.Response, string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", authHeader)
	for k, v := range p.extraHeaders {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("%s chat request: %w", p.name, err)
	}
	return resp, authHeader, nil
}

func (p *Provider) consumeStream(body io.Reader, ch chan<- provider.StreamEvent) {
	reader := bufio.NewReader(body)

	var textBuf strings.Builder
	// Tool calls are emitted via indexed deltas — arguments arrive as concatenated
	// JSON string fragments. Keyed by the choice index.
	partial := map[int]*streamToolCall{}
	// Preserve emission order (by Index) so we hand back tools in the same order
	// OpenAI produced them.
	var order []int
	orderSeen := map[int]bool{}
	var finishReason string

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					break
				}
				var chunk streamChunk
				if jsonErr := json.Unmarshal([]byte(payload), &chunk); jsonErr == nil {
					for _, choice := range chunk.Choices {
						if choice.Delta.Content != "" {
							textBuf.WriteString(choice.Delta.Content)
							ch <- provider.StreamEvent{Type: provider.EventText, TextDelta: choice.Delta.Content}
						}
						for _, tc := range choice.Delta.ToolCalls {
							if !orderSeen[tc.Index] {
								orderSeen[tc.Index] = true
								order = append(order, tc.Index)
							}
							existing, ok := partial[tc.Index]
							if !ok {
								existing = &streamToolCall{Index: tc.Index}
								partial[tc.Index] = existing
							}
							if tc.ID != "" {
								existing.ID = tc.ID
							}
							if tc.Function.Name != "" {
								existing.Function.Name = tc.Function.Name
							}
							existing.Function.Arguments += tc.Function.Arguments
						}
						if choice.FinishReason != "" {
							finishReason = choice.FinishReason
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			ch <- provider.StreamEvent{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
			return
		}
	}

	finalMsg := provider.Message{Role: provider.RoleAssistant}
	if text := textBuf.String(); text != "" {
		finalMsg.Content = append(finalMsg.Content, provider.ContentBlock{Type: provider.BlockText, Text: text})
	}
	for _, idx := range order {
		tc := partial[idx]
		var input map[string]any
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		if input == nil {
			input = map[string]any{}
		}
		tu := &provider.ToolUseBlock{ID: tc.ID, Name: tc.Function.Name, Input: input}
		finalMsg.Content = append(finalMsg.Content, provider.ContentBlock{Type: provider.BlockToolUse, ToolUse: tu})
		ch <- provider.StreamEvent{Type: provider.EventToolUse, ToolUse: tu}
	}

	ch <- provider.StreamEvent{
		Type:         provider.EventStop,
		StopReason:   mapFinishReason(finishReason),
		FinalMessage: &finalMsg,
	}
}

func mapFinishReason(r string) provider.StopReason {
	switch r {
	case "tool_calls", "function_call":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	default:
		return provider.StopEndTurn
	}
}

func buildRequestBody(req provider.SendRequest) ([]byte, error) {
	messages := make([]chatMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, chatMessage{Role: "system", Content: req.SystemPrompt})
	}
	for i, m := range req.Messages {
		msgs, err := toOpenAIMessages(m)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		messages = append(messages, msgs...)
	}

	tools := make([]chatTool, len(req.Tools))
	for i, t := range req.Tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools[i] = chatTool{
			Type: "function",
			Function: chatToolFn{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		}
	}

	return json.Marshal(chatRequest{
		Model:     req.Model,
		Messages:  messages,
		Tools:     tools,
		Stream:    true,
		MaxTokens: req.MaxTokens,
	})
}

// toOpenAIMessages converts a neutral Message to one or more OpenAI chat messages.
// Anthropic represents tool results as ContentBlocks inside a user message; OpenAI
// requires each tool result to be its own message with role="tool". We expand here.
func toOpenAIMessages(m provider.Message) ([]chatMessage, error) {
	switch m.Role {
	case provider.RoleUser:
		var text strings.Builder
		var out []chatMessage
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(b.Text)
			case provider.BlockToolResult:
				if b.ToolResult == nil {
					return nil, fmt.Errorf("tool_result block missing ToolResult")
				}
				out = append(out, chatMessage{
					Role:       "tool",
					ToolCallID: b.ToolResult.ToolUseID,
					Content:    b.ToolResult.Content,
				})
			default:
				return nil, fmt.Errorf("unexpected block type %q in user message", b.Type)
			}
		}
		if text.Len() > 0 {
			// If both text and tool_result blocks exist, OpenAI expects the text
			// as a separate user message preceding the tool messages.
			out = append([]chatMessage{{Role: "user", Content: text.String()}}, out...)
		}
		return out, nil

	case provider.RoleAssistant:
		msg := chatMessage{Role: "assistant"}
		var text strings.Builder
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				text.WriteString(b.Text)
			case provider.BlockToolUse:
				if b.ToolUse == nil {
					return nil, fmt.Errorf("tool_use block missing ToolUse")
				}
				argsJSON, _ := json.Marshal(b.ToolUse.Input)
				msg.ToolCalls = append(msg.ToolCalls, toolCall{
					ID:   b.ToolUse.ID,
					Type: "function",
					Function: toolCallFunc{
						Name:      b.ToolUse.Name,
						Arguments: string(argsJSON),
					},
				})
			default:
				return nil, fmt.Errorf("unexpected block type %q in assistant message", b.Type)
			}
		}
		if text.Len() > 0 {
			msg.Content = text.String()
		}
		return []chatMessage{msg}, nil

	default:
		return nil, fmt.Errorf("unknown role %q", m.Role)
	}
}
