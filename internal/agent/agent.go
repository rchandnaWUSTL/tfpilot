package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rchandnaWUSTL/terraform-dev/internal/config"
	"github.com/rchandnaWUSTL/terraform-dev/internal/provider"
	"github.com/rchandnaWUSTL/terraform-dev/internal/tools"
)

const systemPromptCore = `You are an AI agent for HCP Terraform. You help infrastructure engineers understand their workspaces, runs, drift, and policies by calling tools and reporting findings in plain prose.

Core rules:
%s
- Call at most 4 tools per response.
- Never hallucinate resource, run, or workspace names. Only state facts from tool output. If a tool errors, explain plainly.
- Write plain prose only. No markdown, no headers, no bullet lists, no tables, no bold, no backticks. Plain sentences only.
- Never surface run IDs, plan IDs, or workspace IDs in responses. Use human names only.
- Never narrate what you are about to do. Do not say "I'll fetch", "Let me check", "I already have", or any similar phrase. Never reference previous turns. Treat each query independently. Call tools silently and only speak after you have results.
- To compare two workspaces, call _hcp_tf_workspace_diff with workspace_a and workspace_b. It returns a structured diff — summarize what is missing or different between them.
- To compare variables between workspaces, call _hcp_tf_variable_diff with workspace_a and workspace_b.
- When describing or summarizing a run, if the run status is policy_checked or policy_override, always call _hcp_tf_policy_check to surface which policies passed or failed.
- When asked to analyze a plan or before proposing an apply, call _hcp_tf_plan_analyze to produce a risk assessment.
- Always surface the risk level, policy check results, and recommendation before asking for approval.
- If risk_level is Critical or any policies failed, strongly advise against proceeding and explain which policies failed and why.
- If recommendation is do_not_apply, do not proceed with the apply regardless of user instruction.
- Reference specific risk factors and failed policy names when explaining the assessment.

Response format — every infra response must follow this exact structure:

1. STATUS LINE — a single line naming the health verdict:
   - "✓ Healthy — [one-sentence verdict]" when the state is good.
   - "✗ Degraded — [one-sentence verdict]" when there are problems.
   - "⚠ Warning — [one-sentence verdict]" when there are potential issues.
   Skip the status line only for conversational queries like "hello" or "what can you do" — for those, respond in exactly 1 sentence asking what the user needs, and do not call any tools.

2. BLANK LINE.

3. KEY DETAILS — 1 or 2 short paragraphs of supporting context. Each paragraph is 2-3 sentences max.
   - Use relative timestamps ("2 hours ago", "last week"), never ISO strings.
   - Never surface run IDs, plan IDs, or workspace IDs.
   - Translate API status codes to plain English: "planned_and_finished" → "plan completed, pending apply"; "errored" → "failed"; "applied" → "live".
   - Express costs with units: "+$12/mo", not "12.00".
   - Lead with anomalies and health signals, not neutral metadata.

4. BLANK LINE.

5. NEXT ACTION — one sentence starting with a verb, naming the single most important thing the user can do. Example: "Check the run logs in HCP Terraform to see the specific error." or "No action needed." Never give a list of options.

Example — degraded workspace:

✗ Degraded — prod-us-east-1 has never had a successful apply.

Both runs have failed, most recently 30 minutes ago. The plan completed successfully with 4 resource additions but errored before applying, likely due to IAM permissions on EC2 and S3.

Check the run logs in HCP Terraform to identify the specific IAM error.

Example — healthy workspace:

✓ Healthy — prod-us-east-1 is running cleanly.

Manages 12 resources across AWS (EC2, RDS, ALB). Last applied 2 hours ago with no changes. Auto-apply is off.

No action needed.`

const modeRulesReadonly = `- READ-ONLY mode. Never trigger a run, apply, plan, or mutation.`

const modeRulesApply = `- APPLY mode is enabled. You may propose creating and applying runs.
- Always call _hcp_tf_plan_summary first to show the user what will change before proposing an apply.
- Never call _hcp_tf_run_apply without first showing the plan summary and receiving explicit user confirmation through the approval gate.
- If the plan has destructions > 0, warn the user explicitly before proceeding.
- Always call _hcp_tf_run_discard if the user cancels after a run has been created.`

const configGenRules = `

Config generation:
- When the user asks to generate, create, or add Terraform configuration, emit the HCL inside a fenced code block tagged hcl or terraform. The first line of the block may contain a comment like "# filename: main.tf" to choose the file name; otherwise the content is written to suggested_config.tf in the current working directory.
- The REPL saves the code block to disk and automatically calls _hcp_tf_config_validate against the directory. If validation reports errors, revise the config and re-emit a corrected block.
- After the code is shown, offer the user two options in plain prose: (A) the files are already saved locally, (B) open a pull request by asking you to call _hcp_tf_pr_create with a branch name and commit message.
- Never generate config that includes hardcoded credentials, account IDs, or other sensitive values — use variables and reference them from the workspace's existing variable set.
- Generated config must follow HashiCorp style: 2-space indentation, variables at the top of the file, resources before data sources.`

func buildSystemPrompt(readonly bool) string {
	rules := modeRulesApply
	if readonly {
		rules = modeRulesReadonly
	}
	return fmt.Sprintf(systemPromptCore, rules) + configGenRules
}

// ApprovalFunc is invoked before a mutating tool executes. Returning false
// cancels the tool call — the agent will see a user_cancelled error result.
// The REPL implements this to prompt the user synchronously.
type ApprovalFunc func(name string, args map[string]string) bool

type StreamChunk struct {
	Text string
	Done bool
	Err  error
}

type ToolCallEvent struct {
	Name string
	Args map[string]string
}

type Agent struct {
	prov    provider.Provider
	cfg     *config.Config
	history []provider.Message
}

func New(cfg *config.Config, prov provider.Provider) *Agent {
	return &Agent{prov: prov, cfg: cfg}
}

func (a *Agent) Ask(
	ctx context.Context,
	userMsg string,
	org, workspace string,
	onToolCall func(ToolCallEvent),
	onToolResult func(name string, result *tools.CallResult),
	onApproval ApprovalFunc,
) (<-chan StreamChunk, error) {
	msg := userMsg
	if org != "" || workspace != "" {
		msg = fmt.Sprintf("[Context: org=%s workspace=%s]\n\n%s", org, workspace, userMsg)
	}

	a.history = append(a.history, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: msg}},
	})

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)

		for range 4 {
			done, err := a.runTurn(ctx, onToolCall, onToolResult, onApproval, ch)
			if err != nil {
				ch <- StreamChunk{Err: err}
				return
			}
			if done {
				return
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

// runTurn executes one provider call. Returns true when the agent is done.
func (a *Agent) runTurn(
	ctx context.Context,
	onToolCall func(ToolCallEvent),
	onToolResult func(name string, result *tools.CallResult),
	onApproval ApprovalFunc,
	ch chan<- StreamChunk,
) (done bool, err error) {
	req := provider.SendRequest{
		Model:        a.cfg.Model,
		SystemPrompt: buildSystemPrompt(a.cfg.Readonly),
		Messages:     a.history,
		Tools:        toolDefinitions(a.cfg.Readonly),
		MaxTokens:    a.cfg.MaxTokens,
	}

	events, err := a.prov.SendMessage(ctx, req)
	if err != nil {
		return false, fmt.Errorf("send message: %w", err)
	}

	var finalMsg *provider.Message
	var stop provider.StopReason
	for ev := range events {
		switch ev.Type {
		case provider.EventText:
			ch <- StreamChunk{Text: ev.TextDelta}
		case provider.EventToolUse:
			// Tool execution is deferred until we have the final assistant
			// message — we still need to append that to history before we can
			// append the corresponding tool_result user message.
		case provider.EventStop:
			finalMsg = ev.FinalMessage
			stop = ev.StopReason
		case provider.EventError:
			return false, ev.Err
		}
	}

	if finalMsg == nil {
		return false, fmt.Errorf("provider closed stream without final message")
	}

	a.history = append(a.history, *finalMsg)

	if stop != provider.StopToolUse {
		ch <- StreamChunk{Done: true}
		return true, nil
	}

	var resultBlocks []provider.ContentBlock
	for _, block := range finalMsg.Content {
		if block.Type != provider.BlockToolUse || block.ToolUse == nil {
			continue
		}
		tu := block.ToolUse

		strArgs := toStringMap(tu.Input)
		if onToolCall != nil {
			onToolCall(ToolCallEvent{Name: tu.Name, Args: strArgs})
		}

		var result *tools.CallResult
		if tools.IsMutating(tu.Name) && onApproval != nil && !onApproval(tu.Name, strArgs) {
			result = &tools.CallResult{
				ToolName: tu.Name,
				Args:     strArgs,
				Err: &tools.ToolError{
					ErrorCode: "user_cancelled",
					Message:   "user cancelled the operation at the approval gate",
					Retryable: false,
				},
			}
			tools.LogCancellation(tu.Name, strArgs, result)
		} else {
			result = tools.Call(ctx, tu.Name, strArgs, a.cfg.TimeoutSeconds)
		}
		if onToolResult != nil {
			onToolResult(tu.Name, result)
		}

		var content string
		isError := false
		if result.Err != nil {
			errJSON, _ := json.Marshal(result.Err)
			content = string(errJSON)
			isError = true
		} else {
			content = string(result.Output)
		}

		resultBlocks = append(resultBlocks, provider.ContentBlock{
			Type: provider.BlockToolResult,
			ToolResult: &provider.ToolResultBlock{
				ToolUseID: tu.ID,
				Content:   content,
				IsError:   isError,
			},
		})
	}

	a.history = append(a.history, provider.Message{
		Role:    provider.RoleUser,
		Content: resultBlocks,
	})
	return false, nil
}

func (a *Agent) Reset() {
	a.history = nil
}

func toolDefinitions(readonly bool) []provider.ToolDefinition {
	defs := tools.DefinitionsFor(readonly)
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

func toStringMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
