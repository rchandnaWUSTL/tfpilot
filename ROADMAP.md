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

## v0.2 — Model Provider Abstraction
- Abstract the agent loop behind a ModelProvider interface
- Implementations: Anthropic (current), OpenAI-compatible (new)
- Config: `model_provider: anthropic | openai` in ~/.terraform-dev/config.yaml
- Normalize tool call schema at the provider boundary
- No user-facing changes; sets up v0.3

## v0.3 — GitHub Copilot Auth
- HashiCorp employees already have Copilot licenses — zero adoption barrier
- `terraform dev --auth=copilot` uses existing Copilot credentials
- Copilot uses OpenAI-compatible API format; rides on v0.2 abstraction
- Reference: opencode OSS implementation for credential flow design
- Goal: internal HashiCorp adoption with no new API keys required

## v0.4 — Richer Data Surface
- Multi-workspace diff with real org data (prod vs staging)
- Run history with cost delta and resource change breakdown
- Variable diff across workspaces
- Policy check results integrated into run summaries

## v0.5 — Apply Support
- `--apply` flag unlocks mutation mode
- Dry-run gate: agent proposes, shows plan summary, waits for explicit yes
- Structured approval: natural language rationale + confirmation before apply
- Blast radius check before any apply
- Full audit trail for all mutations

## v1.0 — Public Launch
- GoReleaser pipeline with binaries for Mac/Linux/Windows
- Homebrew tap: `brew install hashicorp/tap/terraform-dev`
- `terraform dev` works as a Terraform CLI subcommand
- Public README, demo GIF, and docs site
