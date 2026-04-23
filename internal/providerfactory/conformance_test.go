package providerfactory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/anthropic"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/openai"
	"github.com/rchandnaWUSTL/tfpilot/internal/tools"
)

// Conformance test: for each tool, we feed each provider a canned response
// representing the model invoking that tool. Both providers must emit the
// same neutral StreamEvents. This is what gives us confidence that v0.3
// Copilot — which reuses the OpenAI provider — will drive the agent loop
// identically to Anthropic.

type toolCase struct {
	toolName string
	input    map[string]any
}

var conformanceCases = []toolCase{
	{"_hcp_tf_runs_list_recent", map[string]any{"org": "o", "workspace": "w"}},
	{"_hcp_tf_workspace_diff", map[string]any{"org": "o", "workspace_a": "a", "workspace_b": "b"}},
	{"_hcp_tf_workspace_describe", map[string]any{"org": "o", "workspace": "w"}},
	{"_hcp_tf_variable_diff", map[string]any{"org": "o", "workspace_a": "a", "workspace_b": "b"}},
	{"_hcp_tf_drift_detect", map[string]any{"org": "o", "workspace": "w"}},
	{"_hcp_tf_policy_check", map[string]any{"run_id": "run-abc"}},
	{"_hcp_tf_plan_summary", map[string]any{"run_id": "run-abc"}},
}

const (
	conformanceText = "Hello world"
	preText         = "Hello"
	postText        = " world"
)

func TestProviderConformance_AllTools(t *testing.T) {
	neutralTools := buildNeutralTools()

	for _, tc := range conformanceCases {
		t.Run(tc.toolName, func(t *testing.T) {
			req := provider.SendRequest{
				Model:        "test-model",
				SystemPrompt: "You are a test.",
				Messages: []provider.Message{{
					Role:    provider.RoleUser,
					Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "test"}},
				}},
				Tools:     neutralTools,
				MaxTokens: 1024,
			}

			anthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeAnthropicSSE(w, tc)
			}))
			defer anthSrv.Close()

			oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeOpenAISSE(w, tc)
			}))
			defer oaiSrv.Close()

			anthProv := anthropic.New(anthropic.Options{BaseURL: anthSrv.URL, APIKey: "test-key"})
			oaiProv := openai.New(openai.Options{Name: "openai", BaseURL: oaiSrv.URL, APIKey: "test-key"})

			anthEvents := collect(t, anthProv, req)
			oaiEvents := collect(t, oaiProv, req)

			assertEquivalent(t, tc, anthEvents, oaiEvents)
		})
	}
}

func buildNeutralTools() []provider.ToolDefinition {
	defs := tools.Definitions()
	out := make([]provider.ToolDefinition, len(defs))
	for i, d := range defs {
		out[i] = provider.ToolDefinition{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		}
	}
	return out
}

func collect(t *testing.T, p provider.Provider, req provider.SendRequest) []provider.StreamEvent {
	t.Helper()
	ch, err := p.SendMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("SendMessage (%s): %v", p.Name(), err)
	}
	var out []provider.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// assertEquivalent collapses the two event streams into comparable normalized
// summaries: concatenated text, ordered list of tool uses, stop reason, and
// the FinalMessage structure. We don't require byte-identical event sequences
// (text deltas may be chunked differently) — we require identical observable
// behavior. Tool-use IDs are provider-native opaque strings ("tu_*" for
// Anthropic, "call_*" for OpenAI) — the agent loop treats them as round-trip
// tokens only, so the conformance check zeroes them out before comparing.
func assertEquivalent(t *testing.T, tc toolCase, a, b []provider.StreamEvent) {
	t.Helper()

	normA := normalize(a)
	normB := normalize(b)
	stripToolUseIDs(&normA)
	stripToolUseIDs(&normB)

	if normA.text != normB.text {
		t.Errorf("concatenated text differs:\n  anthropic: %q\n  openai:    %q", normA.text, normB.text)
	}
	if normA.stop != normB.stop {
		t.Errorf("stop reason differs: anthropic=%q openai=%q", normA.stop, normB.stop)
	}
	if !reflect.DeepEqual(normA.toolUses, normB.toolUses) {
		t.Errorf("tool_use events differ:\n  anthropic: %+v\n  openai:    %+v", normA.toolUses, normB.toolUses)
	}
	if normA.text != conformanceText {
		t.Errorf("anthropic text mismatch: got %q want %q", normA.text, conformanceText)
	}
	if len(normA.toolUses) != 1 {
		t.Fatalf("anthropic expected 1 tool use, got %d", len(normA.toolUses))
	}
	if normA.toolUses[0].Name != tc.toolName {
		t.Errorf("anthropic tool name: got %q want %q", normA.toolUses[0].Name, tc.toolName)
	}
	if !reflect.DeepEqual(normA.toolUses[0].Input, tc.input) {
		t.Errorf("anthropic tool input: got %+v want %+v", normA.toolUses[0].Input, tc.input)
	}
	// Tool IDs must still be non-empty in raw (un-normalized) output so the
	// agent can correlate tool_result blocks — assert on the raw events.
	if anthID := firstToolUseID(a); anthID == "" {
		t.Errorf("anthropic produced empty tool_use ID")
	}
	if oaiID := firstToolUseID(b); oaiID == "" {
		t.Errorf("openai produced empty tool_use ID")
	}
	if normA.stop != provider.StopToolUse {
		t.Errorf("stop reason: got %q want %q", normA.stop, provider.StopToolUse)
	}

	// Final message structure must match across providers.
	if !reflect.DeepEqual(normA.finalMsg, normB.finalMsg) {
		t.Errorf("final messages differ:\n  anthropic: %+v\n  openai:    %+v", normA.finalMsg, normB.finalMsg)
	}
}

type normalized struct {
	text     string
	toolUses []provider.ToolUseBlock
	stop     provider.StopReason
	finalMsg provider.Message
}

func stripToolUseIDs(n *normalized) {
	for i := range n.toolUses {
		n.toolUses[i].ID = ""
	}
	for i, b := range n.finalMsg.Content {
		if b.ToolUse != nil {
			copy := *b.ToolUse
			copy.ID = ""
			n.finalMsg.Content[i].ToolUse = &copy
		}
	}
}

func firstToolUseID(events []provider.StreamEvent) string {
	for _, ev := range events {
		if ev.Type == provider.EventToolUse && ev.ToolUse != nil {
			return ev.ToolUse.ID
		}
	}
	return ""
}

func normalize(events []provider.StreamEvent) normalized {
	var n normalized
	var buf strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case provider.EventText:
			buf.WriteString(ev.TextDelta)
		case provider.EventToolUse:
			if ev.ToolUse != nil {
				n.toolUses = append(n.toolUses, *ev.ToolUse)
			}
		case provider.EventStop:
			n.stop = ev.StopReason
			if ev.FinalMessage != nil {
				n.finalMsg = *ev.FinalMessage
				// Dereference ToolUse pointers for DeepEqual to compare values.
				for i, b := range n.finalMsg.Content {
					if b.ToolUse != nil {
						copy := *b.ToolUse
						n.finalMsg.Content[i].ToolUse = &copy
					}
				}
			}
		}
	}
	n.text = buf.String()
	return n
}

func writeAnthropicSSE(w http.ResponseWriter, tc toolCase) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		flusher = noopFlusher{}
	}
	inputJSON, _ := json.Marshal(tc.input)

	events := []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+preText+`"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+postText+`"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":%q,"input":{}}}`, tc.toolName)),
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%q}}`, string(inputJSON))),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
	for _, ev := range events {
		io.WriteString(w, ev)
		flusher.Flush()
	}
}

func writeOpenAISSE(w http.ResponseWriter, tc toolCase) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		flusher = noopFlusher{}
	}
	argsJSON, _ := json.Marshal(tc.input)

	chunks := []string{
		fmt.Sprintf(`{"choices":[{"delta":{"role":"assistant","content":%q}}]}`, preText),
		fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, postText),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":%q,"arguments":""}}]}}]}`, tc.toolName),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, string(argsJSON)),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, c := range chunks {
		io.WriteString(w, "data: "+c+"\n\n")
		flusher.Flush()
	}
	io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

type noopFlusher struct{}

func (noopFlusher) Flush() {}
