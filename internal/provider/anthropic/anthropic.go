package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
)

type Provider struct {
	client  sdk.Client
	baseURL string
	apiKey  string
}

type Options struct {
	BaseURL string // optional override, used by tests
	APIKey  string // optional override; defaults to ANTHROPIC_API_KEY env var
}

func New(opts Options) *Provider {
	key := opts.APIKey
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}

	clientOpts := []option.RequestOption{option.WithAPIKey(key)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}

	return &Provider{
		client:  sdk.NewClient(clientOpts...),
		baseURL: opts.BaseURL,
		apiKey:  key,
	}
}

func (p *Provider) Name() string { return "anthropic" }

func (p *Provider) Authenticate(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("✗ ANTHROPIC_API_KEY not found in environment.\n    export ANTHROPIC_API_KEY=your-key")
	}
	return nil
}

func (p *Provider) SendMessage(ctx context.Context, req provider.SendRequest) (<-chan provider.StreamEvent, error) {
	msgs, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("translate messages: %w", err)
	}
	tools := toAnthropicTools(req.Tools)

	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
		Tools:     tools,
	}
	if req.SystemPrompt != "" {
		params.System = []sdk.TextBlockParam{{Type: "text", Text: req.SystemPrompt}}
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	ch := make(chan provider.StreamEvent, 64)

	go func() {
		defer close(ch)

		var acc sdk.Message
		for stream.Next() {
			event := stream.Current()
			if err := acc.Accumulate(event); err != nil {
				ch <- provider.StreamEvent{Type: provider.EventError, Err: fmt.Errorf("accumulate: %w", err)}
				return
			}
			if cbDelta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent); ok {
				if textDelta, ok := cbDelta.Delta.AsAny().(sdk.TextDelta); ok && textDelta.Text != "" {
					ch <- provider.StreamEvent{Type: provider.EventText, TextDelta: textDelta.Text}
				}
			}
		}
		if err := stream.Err(); err != nil {
			ch <- provider.StreamEvent{Type: provider.EventError, Err: fmt.Errorf("stream error: %w", err)}
			return
		}

		finalMsg := provider.Message{Role: provider.RoleAssistant}
		for _, block := range acc.Content {
			switch b := block.AsAny().(type) {
			case sdk.TextBlock:
				finalMsg.Content = append(finalMsg.Content, provider.ContentBlock{
					Type: provider.BlockText,
					Text: b.Text,
				})
			case sdk.ToolUseBlock:
				var input map[string]any
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				tu := &provider.ToolUseBlock{ID: b.ID, Name: b.Name, Input: input}
				finalMsg.Content = append(finalMsg.Content, provider.ContentBlock{
					Type:    provider.BlockToolUse,
					ToolUse: tu,
				})
				ch <- provider.StreamEvent{Type: provider.EventToolUse, ToolUse: tu}
			}
		}

		stop := mapStopReason(acc.StopReason)
		ch <- provider.StreamEvent{
			Type:         provider.EventStop,
			StopReason:   stop,
			FinalMessage: &finalMsg,
		}
	}()

	return ch, nil
}

func mapStopReason(r sdk.StopReason) provider.StopReason {
	switch r {
	case sdk.StopReasonToolUse:
		return provider.StopToolUse
	case sdk.StopReasonMaxTokens:
		return provider.StopMaxTokens
	default:
		return provider.StopEndTurn
	}
}

func toAnthropicTools(defs []provider.ToolDefinition) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, len(defs))
	for i, d := range defs {
		schemaJSON, _ := json.Marshal(d.InputSchema)
		var schema sdk.ToolInputSchemaParam
		_ = json.Unmarshal(schemaJSON, &schema)
		out[i] = sdk.ToolUnionParamOfTool(schema, d.Name)
		out[i].OfTool.Description = sdk.String(d.Description)
	}
	return out
}

func toAnthropicMessages(msgs []provider.Message) ([]sdk.MessageParam, error) {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for i, m := range msgs {
		blocks, err := toAnthropicBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		switch m.Role {
		case provider.RoleUser:
			out = append(out, sdk.NewUserMessage(blocks...))
		case provider.RoleAssistant:
			out = append(out, sdk.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
	}
	return out, nil
}

func toAnthropicBlocks(blocks []provider.ContentBlock) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for j, b := range blocks {
		switch b.Type {
		case provider.BlockText:
			out = append(out, sdk.NewTextBlock(b.Text))
		case provider.BlockToolUse:
			if b.ToolUse == nil {
				return nil, fmt.Errorf("block %d: tool_use missing ToolUse", j)
			}
			input := b.ToolUse.Input
			if input == nil {
				input = map[string]any{}
			}
			out = append(out, sdk.NewToolUseBlock(b.ToolUse.ID, input, b.ToolUse.Name))
		case provider.BlockToolResult:
			if b.ToolResult == nil {
				return nil, fmt.Errorf("block %d: tool_result missing ToolResult", j)
			}
			out = append(out, sdk.NewToolResultBlock(b.ToolResult.ToolUseID, b.ToolResult.Content, b.ToolResult.IsError))
		default:
			return nil, fmt.Errorf("block %d: unknown type %q", j, b.Type)
		}
	}
	return out, nil
}
