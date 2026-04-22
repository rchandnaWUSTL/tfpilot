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

## v0.4 — Richer Data Surface (in progress)
Done:
- Multi-workspace diff with real resource addresses (prod vs staging)
- workspace_describe returns actual resource types and inventory

Remaining:
- Variable diff across workspaces
- Policy check results integrated into run summaries

## v0.5 — Apply Support
- `--apply` flag unlocks mutation mode
- Dry-run gate: agent proposes, shows plan summary, waits for explicit yes
- Structured approval: natural language rationale + confirmation before apply
- Blast radius check before any apply
- Before any apply, fetch and display cost estimate: "+$X/mo" based on plan resource additions/changes
- Surface cost delta in run summaries when plan data is available
- Full audit trail for all mutations

## v0.6 — Config Generation
- Natural language → Terraform config
- "Add a WAF to all public ALBs" → agent generates .tf files, opens a PR
- Option A: write locally to current directory
- Option B: push directly to VCS repo connected to the workspace (HCP Terraform VCS trigger picks it up automatically)
- Validate with hcptf plans create --dry-run before proposing
- Full loop: intent → config → plan → approval → PR

## v0.7 — Application-Aware Infrastructure Generation
- User runs terraform dev in their application repo root
- Agent scans the directory — infers runtime, dependencies, and resource requirements from package.json, Dockerfile, requirements.txt, etc.
- Generates full Terraform config to deploy the application: EKS cluster, ECR repo, ALB, IAM roles, VPC
- Plans against HCP Terraform workspace before proposing
- Opens a PR to the connected VCS repo
- Requires v0.6 config generation foundation

## v1.0 — Public Launch
- GoReleaser pipeline with binaries for Mac/Linux/Windows
- Homebrew tap: `brew install hashicorp/tap/terraform-dev`
- `terraform dev` works as a Terraform CLI subcommand
- Public README, demo GIF, and docs site
- GitHub Marketplace listing (requires Copilot auth + Homebrew tap first)
- List application at: https://github.com/marketplace
