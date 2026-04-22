# Terraform Dev — Roadmap

## v0.1 — Shipped
- Working REPL with `terraform dev` entrypoint and `hcp-tf>` prompt
- 6 read-only tools backed by hcptf CLI
- Streaming agent loop (Claude Sonnet 4.6 via Anthropic API)
- HashiCorp brand color palette throughout
- Structured response format: status line, details, next action
- Real workspace diff with parallel state fetch
- Auth gate: hcptf credentials + ANTHROPIC_API_KEY checked at startup
- Audit log at ~/.terraform-dev/audit.log

## v0.2 — Model Provider Abstraction — Shipped
- Abstract the agent loop behind a ModelProvider interface
- Implementations: Anthropic (current), OpenAI-compatible (new)
- Config: `model_provider: anthropic | openai` in ~/.terraform-dev/config.yaml
- Normalize tool call schema at the provider boundary
- No user-facing changes; sets up v0.3

Note: Revisit adopting opencode's provider framework when a third provider is needed. Current abstraction works well for two providers; framework adoption trades control for convenience.

## v0.3 — GitHub Copilot Auth — Shipped
- HashiCorp employees already have Copilot licenses — zero adoption barrier
- `terraform dev --auth=copilot` uses existing Copilot credentials
- Copilot uses OpenAI-compatible API format; rides on v0.2 abstraction
- Reference: opencode OSS implementation for credential flow design
- Goal: internal HashiCorp adoption with no new API keys required

## Also shipped (v0.1–v0.3 improvements)
- Real workspace diff using hcptf workspace resource list — actual resource addresses instead of state version metadata
- workspace_describe enriched with actual resource types and full inventory
- Markdown stripping in agent responses
- HTML error normalization across all 6 tools
- Structured response format: status line, details, next action
- HashiCorp brand color palette throughout terminal UI
- Getting Started section in README with binary install instructions
- Copilot token cached at ~/.terraform-dev/copilot.json with auto-refresh on 401

## v0.4 — Richer Data Surface (Shipped)
- Multi-workspace diff with real resource addresses
- workspace_describe returns actual resource types and inventory  
- Variable diff across workspaces (_hcp_tf_variable_diff)
- Policy check integration in run summaries
- Empty variable list handled correctly

## v0.5 — Apply Support (Shipped)
- --apply flag unlocks mutation mode (readonly is default)
- 3 new mutating tools: _hcp_tf_run_create, _hcp_tf_run_apply, _hcp_tf_run_discard
- Synchronous approval gate before every mutation
- Blast radius check: second "yes" required when plan has destructions > 0
- Auto-discard on cancel if a run was already created
- Audit log at ~/.terraform-dev/audit.log for every tool call
- Cost estimate surfaced in plan summary when available
- Mutating tools invisible to model in readonly mode

## v0.6 — Config Generation (Shipped)
- Natural language → Terraform config
- Agent emits HCL in fenced code blocks; REPL extracts and writes to cwd
- _hcp_tf_config_validate runs `terraform validate -json` (with best-effort init)
- _hcp_tf_pr_create creates a branch, commits, pushes, and opens a PR via gh
- Existing-file overwrite protection (prompt before replacing)
- Validation runs automatically after file write
- Clean error codes when terraform or gh is missing from PATH

## v0.7 — Plan Analyzer (Shipped)
- _hcp_tf_plan_analyze tool with risk scoring heuristics
- Risk levels: Low / Medium / High / Critical with color coding
- Blast radius: total resources affected, additions/changes/destructions breakdown
- Policy pre-check: surfaces failed policies, triggers Critical risk level
- /analyze <run-id> slash command for direct plan analysis
- Apply gate integrated: risk level determines confirmation requirements (single yes / double yes / workspace name)
- Failed policies always trigger Critical regardless of other factors

## v0.8 — Application-Aware Infrastructure Generation
- User runs terraform dev in their application repo root
- Agent scans the directory — infers runtime, dependencies, and resource requirements from package.json, Dockerfile, requirements.txt, etc.
- Generates full Terraform config to deploy the application: EKS cluster, ECR repo, ALB, IAM roles, VPC
- Plans against HCP Terraform workspace before proposing
- Opens a PR to the connected VCS repo
- Requires v0.6 config generation foundation

## v0.9 — Stacks Integration
- Point the agent at the HCP Terraform Stacks knowledge base
- Stack-aware tools: list stacks, describe stack configurations, compare stack deployments
- Natural language queries about stack topology and dependencies
- High internal HashiCorp value — Stacks is where enterprise customers are headed

## v1.0 — Public Launch
- GoReleaser pipeline with binaries for Mac/Linux/Windows
- Homebrew tap: `brew install terraform-dev`
- `terraform dev` works as a Terraform CLI subcommand
- Public README, demo GIF, and docs site
- GitHub Marketplace listing (requires Copilot auth + Homebrew tap first)
- List application at: https://github.com/marketplace

## v1.1 — API Surface + Web UI
- Wrap the agent loop in an HTTP API
- Same tools and agent, accessible from a web chat UI
- Makes terraform-dev accessible to non-terminal users: PMs, security teams, compliance
- Provider abstraction already in place — the terminal is just one UX on top

## v1.2 — Concierge Mode
- Proactive agent that surfaces issues without being asked
- Monitors workspaces for drift, policy failures, cost spikes, expiring credentials
- Sends alerts via Slack or email when thresholds are crossed
- Shift from reactive (answer questions) to proactive (surface problems)
