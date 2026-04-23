# tfpilot — PRD

**Status:** v0.1–v0.6 shipped; v0.7 next
**Owner:** Roshan Chandna
**Team:** PAVE / AGX

---

## 1. Overview

tfpilot is an AI-native terminal REPL that lets infrastructure engineers describe what they want in plain English and have an agent drive HCP Terraform end-to-end: reading configs and state, proposing changes, running plans, interpreting diffs, enforcing policy, applying with guardrails, and reporting back.

You launch it with a single command:

```
$ tfpilot --org=<org> --workspace=<ws> --auth=copilot
```

From there, the prompt replaces raw CLI incantations:

```
hcp-tf> Show me my latest prod run and tell me if it's safe to apply similar changes to staging.
```

The agent calls the right tools, reasons over structured output, and streams a response in plain English with specific next steps — then waits for explicit approval before doing anything destructive.

---

## 2. Problem

Terraform practitioners today face a multi-tool coordination problem. A typical infra change requires:

- Running `terraform plan` and manually inspecting hundreds of lines of diff
- Cross-referencing state files, variable files, and the cloud console to understand actual topology
- Separately checking Sentinel/OPA policy results and cost estimates
- Deciding when to apply based on experience and intuition, not structured analysis
- Doing all of this across multiple workspaces and environments manually

Claude Code can edit `.tf` files and run commands. But it lacks the opinionated tool surface that encodes HCP Terraform concepts: workspaces, runs, drift, cost, policies, identity, and audit. Engineers still manually orchestrate the loop.

The gap is not "LLM plus CLI." The gap is a coherent agent loop that already understands the HCP Terraform data model, respects org identity and policy, and surfaces the right information at the right time.

The differentiated asset is not the agent runtime. It is the tooling contract: agent-first CLI commands that already encode HCP Terraform concepts, respect org identity, produce structured output, and emit audit logs. tfpilot is the thin, opinionated shell on top.

---

## 3. Goals

### Shipped (v0.1–v0.6)

- Working REPL with `tfpilot` entrypoint, `hcp-tf>` prompt, history, and slash commands
- Streaming agent loop with planner + tool-calling + narrative summarizer
- Read-only mode by default; `--apply` flag unlocks mutation with synchronous approval gate
- 9 tools: 6 read-only (runs, workspace describe/diff, variable diff, drift detect, policy check, plan summary) + 3 mutating (run create/apply/discard) + 2 config (validate, PR create)
- Multi-provider: Anthropic (Claude Sonnet 4.6) and OpenAI-compatible (GitHub Copilot)
- `--auth=copilot` uses existing Copilot credentials — zero new API keys for HashiCorp employees
- Natural language → Terraform config with `terraform validate` + optional `gh pr create`
- HashiCorp brand color palette, structured response format (status line, details, next action)
- Auth gate on startup: `hcptf` credential check with inline `hcptf login` fallback
- Audit log at `~/.tfpilot/audit.log` — JSON-per-line, every tool call including approval-gate cancellations

### In scope for v0.7 (next)

- Application-aware infrastructure generation: scan the current directory for runtime hints (`package.json`, `Dockerfile`, `requirements.txt`) and generate full Terraform to deploy the app
- Plan against an HCP Terraform workspace before proposing
- Open a PR to the connected VCS repo

### Explicitly deferred

- MCP server wrapper (build when external consumers exist)
- In-terminal model switcher
- Multi-step approval workflows with structured sign-off and delegation
- Custom policy override flows
- Plan analyzer with blast-radius scoring (v0.8)
- Stacks integration (v0.9)

---

## 4. User Stories

Primary persona: Platform Engineer or Senior DevOps Engineer who owns HCP Terraform at their org.

| As a... | I want to... |
|---|---|
| Platform engineer | Ask in plain English whether it's safe to apply prod changes to staging, and get a structured answer without running 5 CLI commands. |
| DevOps lead | Describe a topology change in English, see the plan summarized as human costs and risks, and approve it — all without writing Terraform. |
| On-call SRE | Ask why an instance is unreachable and get a root-cause analysis pointing at SGs, routes, or IAM — not a list of commands to run myself. |
| Security reviewer | Generate a starter Terraform module from an intent description, validate it locally, and open a PR for peer review — without hand-authoring HCL. |

---

## 5. Architecture

tfpilot has four layers. Each has a clearly scoped responsibility and a clean interface to the next.

### Layer 1: Agent-ready CLI (hcptf)

The foundation is the `hcptf` CLI — a Go binary with 231+ commands across 60+ HCP Terraform resource types. What makes it agent-ready:

- All commands take explicit flags, no interactive prompts
- `-output=json` on every command returns machine-parsable structured data
- `-dry-run` on all mutations: validate without side effects
- `schema` introspection: `hcptf schema <command>` returns flag definitions as JSON
- Consistent exit codes: 0 success, 1 API error, 2 usage error

The CLI is treated as a stable, read-only dependency.

### Layer 2: Tool layer

A thin Go wrapper turns CLI commands into named tools the agent can call. Each tool:

- Has a clear name like `_hcp_tf_runs_list_recent` or `_hcp_tf_workspace_diff`
- Shells out to `hcptf` (or `terraform` / `git` / `gh` for config tools) with the appropriate flags and `-output=json`
- Enforces a timeout (default 10s, overridable per-call)
- Normalizes errors into `{ error_code, message, retryable }` — never passes raw stderr to the model
- Returns only structured JSON — no ANSI color codes, no interactive prompts
- Emits an audit log line for every invocation, including cancellations at the approval gate

The model sees different tool sets based on mode. In readonly mode, mutating tools are filtered out of the definitions entirely — they do not exist from the model's perspective.

### Layer 3: Agent loop

A minimal planner/summarizer loop behind a `ModelProvider` interface:

- **Anthropic** implementation: Claude Sonnet 4.6 via the Messages API
- **OpenAI-compatible** implementation: shared by Copilot and other OpenAI-style providers
- **Planner:** takes the user's natural language request, selects a short ordered toolchain (max 4 tools per turn), calls them in sequence
- **Summarizer:** takes the structured tool outputs and streams a narrative response with risks, costs, and next steps
- **Mode-aware system prompt:** readonly mode forbids mutations; apply mode enables them with explicit approval-gate rules; config generation rules apply in both
- **Approval gate:** before any mutating tool call, the REPL prompts the user synchronously; the agent sees `user_cancelled` as a tool error when declined
- **Streaming:** the agent response streams token-by-token for a terminal-native feel

### Layer 4: Terminal UX (`tfpilot`)

A long-running REPL process that renders the conversation:

- `hcp-tf>` prompt with readline history and basic editing
- `/help`, `/mode`, `/workspace`, `/org`, `/reset`, `/exit` slash commands
- Tool call rendering: spinner + name + flags, then a green checkmark with truncated JSON on success
- AI response streams in white; status lines use HashiCorp brand colors (Terraform purple, Waypoint teal, Vault yellow, Boundary pink)
- `--apply` flag unlocks mutation mode; default is readonly
- `--auth=copilot` selects Copilot provider; default uses `model_provider` from config
- Approval prompts are synchronous with `⚠` warnings in vault yellow and `✗` destruction warnings in boundary pink
- Generated HCL code blocks are extracted from responses and written to the current directory (default `suggested_config.tf`, `# filename:` hint override), with overwrite protection

---

## 6. Tool Surface

### Read-only tools (always available)

| Tool | What it does |
|---|---|
| `_hcp_tf_runs_list_recent` | Lists the N most recent runs for a workspace with status, timestamps, resource counts, and cost delta |
| `_hcp_tf_workspace_describe` | Returns workspace metadata merged with the actual resource inventory (types + count) |
| `_hcp_tf_workspace_diff` | Compares two workspaces' resource addresses in parallel — missing_in_a, missing_in_b, present_in_both |
| `_hcp_tf_variable_diff` | Compares variables between two workspaces; never exposes values — only keys, categories, and sensitive flags |
| `_hcp_tf_drift_detect` | Returns assessment results for a workspace showing detected drift and changed resources |
| `_hcp_tf_policy_check` | Returns policy check results for a run: which checks passed/failed, which rules fired |
| `_hcp_tf_plan_summary` | Returns a summary of a plan: adds/changes/destroys and, when available, a formatted monthly cost delta |

### Mutating tools (only when `--apply` is set)

| Tool | What it does |
|---|---|
| `_hcp_tf_run_create` | Creates a new run in a workspace |
| `_hcp_tf_run_apply` | Applies a previously-created run — the only tool that triggers real infrastructure changes |
| `_hcp_tf_run_discard` | Discards a pending run so it cannot be applied |

### Config generation tools (always available)

| Tool | What it does |
|---|---|
| `_hcp_tf_config_validate` | Runs `terraform validate -json` against a local directory; returns `{ valid, errors }`. Runs best-effort `terraform init -backend=false` first when providers are not yet installed. |
| `_hcp_tf_pr_create` | Creates a branch, commits the specified files, pushes to origin, and opens a PR via `gh` CLI. Returns `{ pr_url, branch }`. |

---

## 7. Example Interactions

### Prod-to-staging safety check

```
hcp-tf> Is it safe to apply my latest prod changes to staging?
```

Agent calls: `_hcp_tf_runs_list_recent` (prod) → `_hcp_tf_workspace_diff` (prod..staging) → `_hcp_tf_policy_check`

### Topology description

```
hcp-tf> Describe the prod-us-east-1 workspace
```

Agent calls: `_hcp_tf_workspace_describe`

### Cancelling a run at the approval gate

```
hcp-tf> Create a new run in prod-k8s-apps
```

Agent calls: `_hcp_tf_plan_summary` (current run) → proposes `_hcp_tf_run_create` → approval gate prompts. User types `no` → REPL prints `Cancelled.` in boundary pink. Any previously-created run is auto-discarded.

### Generating Terraform from intent

```
hcp-tf> Generate a null_resource called hello_world
```

Agent emits HCL in a fenced `hcl` code block → REPL extracts and writes `./suggested_config.tf` → REPL calls `_hcp_tf_config_validate` → agent offers PR option.

---

## 8. Governance Model

Every action runs under the user's scoped HCP Terraform identity (via the existing `hcptf` credential chain).

- **Read-only by default:** no mutations, no plans triggered, no applies. The `--apply` flag must be passed at startup to unlock mutation.
- **Mutating tools invisible in readonly mode:** they are filtered from the tool definitions sent to the model — it never sees them, cannot call them, and cannot be tricked into them.
- **Synchronous approval gate:** before any mutating tool executes, the REPL prompts the user with a warning in vault yellow. The user must type `yes` to proceed; anything else cancels.
- **Blast radius check:** when the last observed plan summary reports destroys > 0, a second confirmation in boundary pink is required before apply.
- **Auto-discard on cancel:** if the user cancels an apply after a run was already created, the REPL invokes `_hcp_tf_run_discard` synchronously so the run does not remain pending.
- **Auth gate on startup:** `hcptf` credential check with inline `hcptf login` fallback; model-provider auth (Anthropic or Copilot) surfaced with a user-friendly error when missing.
- **Audit trail:** every tool call is appended as a JSON line to `~/.tfpilot/audit.log` with timestamp, tool, args, result, and the hcptf user identity. Approval-gate cancellations are logged too, with `error_code: user_cancelled`.

---

## 9. Configuration

Config file at `~/.tfpilot/config.yaml`. Created automatically on first run with defaults.

```yaml
model: claude-sonnet-4-6        # Anthropic default; overridden when --auth=copilot
max_tokens: 16384
timeout_seconds: 10
readonly: true                  # overridden at runtime by --apply
model_provider: anthropic       # or "openai" for OpenAI-compatible endpoints
openai_base_url: ""             # when model_provider=openai
```

Copilot credentials are cached at `~/.tfpilot/copilot.json` with auto-refresh on 401.

---

## 10. Success Metrics

- End-to-end demo runs cleanly against a live HCP Terraform org with real workspaces
- Agent correctly selects the right tool sequence for the 6 canonical demo prompts (discovery, drift, diff, variable diff, apply-with-approval, config generation)
- Tool calls complete in under 5 seconds for read operations
- Zero hallucinated resource names, run IDs, or workspace IDs in agent responses
- AI response streams token-by-token — no waiting for full completion before output appears
- Every mutation is preceded by an explicit approval gate and captured in the audit log
