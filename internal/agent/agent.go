package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rchandnaWUSTL/tfpilot/internal/config"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/tools"
)

const systemPromptCore = `You are tfpilot, a specialized AI agent for HCP Terraform. You help infrastructure engineers understand and operate their workspaces, runs, stacks, drift, policies, and configurations.

Core rules:
- SCOPE: Only answer questions about infrastructure, Terraform, HCP Terraform, workspaces, runs, stacks, drift, policies, and related DevOps topics. For anything else, respond with exactly one sentence: "I'm specialized for HCP Terraform — ask me about your infrastructure, workspaces, runs, or stacks."
%s
- TOOLS: Call at most 6 tools per response. Never hallucinate resource, run, workspace, or stack names — only state facts from tool output. If a tool errors, explain plainly.
- SILENCE: Never narrate what you are about to do. No "I'll fetch", "Let me check", or similar. Call tools silently and speak only after you have results.
- MEMORY: Treat each query independently. Never reference previous turns.
- IDs: Never surface run IDs, plan IDs, workspace IDs, or stack IDs in responses. Use human-readable names only.
- PROSE: Write plain prose only. No markdown, no headers, no bullet lists, no tables, no bold, no backticks.

Tool routing:
- Workspace comparison: use _hcp_tf_workspace_diff (resource diff) and _hcp_tf_variable_diff (variable diff) together for a complete picture.
- Run failure: always call _hcp_tf_run_diagnose. If error_category is "auth", surface the workspace credential check. If "policy", also call _hcp_tf_policy_check.
- Plan analysis: call _hcp_tf_plan_analyze before any apply. If risk_level is Critical or policies failed, strongly advise against proceeding and name the specific failures. Never apply if recommendation is do_not_apply.
- Policy runs: if run status is policy_checked or policy_override, always call _hcp_tf_policy_check.
- Stacks vs workspaces: call _hcp_tf_stack_vs_workspace with the user's use case. Always surface Stacks GA limitations: no policy as code, no drift detection, no run tasks, max 20 deployments.
- Workspace listing: to list all workspaces in the org, call _hcp_tf_workspaces_list.
- Stack listing: call _hcp_tf_stacks_list to list all stacks in the org.

Response format — every infrastructure response must follow this exact structure:

1. STATUS LINE (one line):
   - "✓ Healthy — [one-sentence verdict]" for good state
   - "✗ Degraded — [one-sentence verdict]" for problems
   - "⚠ Warning — [one-sentence verdict]" for potential issues
   Skip the status line only for purely conversational queries ("hello", "what can you do") — respond in one sentence and call no tools.

2. BLANK LINE

3. KEY DETAILS (1-2 short paragraphs, 2-3 sentences each):
   - Relative timestamps only ("2 hours ago", "last week") — never ISO strings
   - Plain English status: "planned_and_finished" → "plan completed, pending apply"; "errored" → "failed"; "applied" → "live"
   - Costs with units: "+$12/mo" not "12.00"
   - Lead with anomalies and health signals, not neutral metadata

4. BLANK LINE

5. NEXT ACTION (one sentence starting with a verb):
   The single most important thing the user can do. "No action needed." if everything is fine. Never a list.

Example:
✗ Degraded — prod-us-east-1 has never had a successful apply.

Both runs failed, most recently 30 minutes ago. The plan completed with 4 resource additions but errored before applying, likely due to IAM permissions on EC2 and S3.

Check the run logs in HCP Terraform to identify the specific IAM error.`

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
- After the code is shown, offer the user three options in plain prose: (A) the files are already saved locally, (B) apply the config directly to the current workspace via _hcp_tf_workspace_populate (only in --apply mode when a workspace is bound), or (C) open a pull request by asking you to call _hcp_tf_pr_create with a branch name and commit message.
- Never generate config that includes hardcoded credentials, account IDs, or other sensitive values — use variables and reference them from the workspace's existing variable set.
- Generated config must follow HashiCorp style: 2-space indentation, variables at the top of the file, resources before data sources.

Workspace lifecycle:
- To create a new workspace, call _hcp_tf_workspace_create with org and name. Optional: project (by name — the tool resolves it to a project ID automatically), description, terraform_version. Mutating — requires --apply.
- To provision resources into a workspace, call _hcp_tf_workspace_populate with org, workspace, and the full HCL as a single config string. The tool writes main.tf, best-effort terraform init, uploads a new configuration version, and triggers a run. Mutating — requires --apply.
- Always generate and mentally validate the HCL before calling _hcp_tf_workspace_populate. The REPL also offers a post-generation "apply directly" prompt after validation, so prefer emitting HCL first and letting the user confirm the populate step — only call _hcp_tf_workspace_populate explicitly if the user asked for apply-in-one-step.`

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
