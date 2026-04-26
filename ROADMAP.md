# tfpilot — Roadmap

## v0.1 — Shipped
- Working REPL with `tfpilot` entrypoint and `hcp-tf>` prompt
- 6 read-only tools backed by hcptf CLI
- Streaming agent loop (Claude Sonnet 4.6 via Anthropic API)
- HashiCorp brand color palette throughout
- Structured response format: status line, details, next action
- Real workspace diff with parallel state fetch
- Auth gate: hcptf credentials + ANTHROPIC_API_KEY checked at startup
- Audit log at ~/.tfpilot/audit.log

## v0.2 — Model Provider Abstraction — Shipped
- Abstract the agent loop behind a ModelProvider interface
- Implementations: Anthropic (current), OpenAI-compatible (new)
- Config: `model_provider: anthropic | openai` in ~/.tfpilot/config.yaml
- Normalize tool call schema at the provider boundary
- No user-facing changes; sets up v0.3

Note: Revisit adopting opencode's provider framework when a third provider is needed. Current abstraction works well for two providers; framework adoption trades control for convenience.

## v0.3 — GitHub Copilot Auth — Shipped
- HashiCorp employees already have Copilot licenses — zero adoption barrier
- `tfpilot --auth=copilot` uses existing Copilot credentials
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
- Copilot token cached at ~/.tfpilot/copilot.json with auto-refresh on 401

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
- Audit log at ~/.tfpilot/audit.log for every tool call
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
- _hcp_tf_plan_analyze tool: risk scoring (Low/Medium/High/Critical), blast radius, policy pre-check
- Risk-level color coding: teal/yellow/pink/pink-bold
- Apply gate integration: confirmation requirements scale with risk level
- /analyze <run-id> slash command for direct plan analysis
- _hcp_tf_run_diagnose tool: error categorization (auth/quota/resource_conflict/provider/config/policy/network/unknown)
- /diagnose <run-id> slash command with formatted output and suggested fixes
- Agent automatically diagnoses failed runs when asked why a run failed
- Auth errors surface workspace credential check suggestion
- Policy errors chain _hcp_tf_policy_check automatically

## v0.8 — Stacks Integration (Shipped)
- _hcp_tf_stacks_list: lists all stacks in org with deployment counts and health
- _hcp_tf_stack_describe: describes stack topology, components, deployments, and GA limitations
- _hcp_tf_stack_vs_workspace: deterministic recommendation engine for stack vs workspace decisions
- /stacks slash command with empty-state guidance and docs link
- Agent surfaces Stacks GA limitations automatically (no policy as code, no drift detection, max 20 deployments)
- Health computed from deployment run status (Unknown when no runs exist)

## v0.9 — Workspace Lifecycle (Shipped)
- _hcp_tf_workspace_create: creates a workspace in an org, resolving a project name to a project_id automatically
- _hcp_tf_workspace_populate: writes HCL to a tempdir, best-effort `terraform init`, uploads a configuration version, and triggers a run in one tool call
- Direct-apply offer in the config-generation flow — after validation succeeds in --apply mode with a bound workspace, the REPL prompts "Apply this config directly to <ws>?" and routes through the mutation approval gate
- Archivist one-shot UploadURL handled by capturing the URL from `configversion create` and PUT-ing the tar.gz directly, since hcptf's `configversion upload` cannot re-fetch it
- Demo environment (prod-api + staging-api in sarah-test-org / zzryan project) provisioned by tfpilot itself as the validation step; see ops/now/v11-workspace-lifecycle-demo-log.md for the durable record

## v0.10 — Run Task Integrations
- _hcp_tf_runtask_list: list run tasks configured on a workspace
- _hcp_tf_runtask_attach: deploy an AI-powered run task integration to a workspace
- Built-in support for two community run task modules:
  - terraform-aws-runtask-tf-plan-analyzer (AWS Bedrock + Claude) — github.com/aws-ia/terraform-aws-runtask-tf-plan-analyzer
  - terraform-google-ai-debugger (Google Vertex AI + Gemini) — github.com/gautambaghel/terraform-google-ai-debugger
- "Attach AI plan analysis to prod-k8s-apps" → tfpilot deploys the run task and wires it to the workspace automatically
- Complements the local _hcp_tf_plan_analyze tool with a server-side, always-on analysis layer
- Requires --apply mode (run task attachment is a mutation)

## v0.11 — Application-Aware Infrastructure Generation
- User runs tfpilot in their application repo root
- Agent scans the directory — infers runtime, dependencies, and resource requirements from package.json, Dockerfile, requirements.txt, etc.
- Generates full Terraform config to deploy the application: EKS cluster, ECR repo, ALB, IAM roles, VPC
- Plans against HCP Terraform workspace before proposing
- Opens a PR to the connected VCS repo
- Requires v0.6 config generation foundation

## v1.0 — Public Launch
- GoReleaser pipeline with binaries for Mac/Linux/Windows
- Homebrew tap: `brew install tfpilot`
- `tfpilot` works as a Terraform CLI subcommand
- Public README, demo GIF, and docs site
- GitHub Marketplace listing (requires Copilot auth + Homebrew tap first)
- List application at: https://github.com/marketplace

## v1.1 — API Surface + Web UI
- Wrap the agent loop in an HTTP API
- Same tools and agent, accessible from a web chat UI
- Makes tfpilot accessible to non-terminal users: PMs, security teams, compliance
- Provider abstraction already in place — the terminal is just one UX on top

## v1.2 — Proactive Monitoring Mode
- Proactive agent that surfaces issues without being asked
- Monitors workspaces for drift, policy failures, cost spikes, expiring credentials
- Sends alerts via Slack or email when thresholds are crossed
- Shift from reactive (answer questions) to proactive (surface problems)

## v1.3 — Adoption Intelligence
- Per-workspace usage metrics surfaced inside the REPL ("this workspace has had 47 tfpilot sessions this month")
- Top questions and workflows identified across the org to guide product priorities
- Team-level rollout tracking: who has authenticated, who has run applies, who has never used tfpilot
- Friction report: prompts that required multiple retries, approval gates that got cancelled, errors that went unresolved

## v1.4 — Workspace Intelligence
- Workspace ownership: who created it, team access, last modified by
- Staleness analysis: workspaces with no runs in N days, drifted resources, abandoned configs
- Persona-aware responses: Admin, Engineer, and App Dev personas get different levels of detail and different suggested next actions

## v1.5 — Org Health Audit (In Progress)
- _hcp_tf_version_audit tool: groups all workspaces by Terraform version, surfaces CVEs from OSV.dev, and scores upgrade complexity per version group
- /audit slash command: human-readable version + CVE summary for the pinned org
- _hcp_tf_module_audit tool: infers Terraform Registry modules from a workspace's resource addresses and queries `hcptf publicregistry` for the latest available version of each known module; modules outside the built-in registry map are surfaced under unknown_modules
- /modules slash command: per-workspace module version report
- _hcp_tf_provider_audit tool: extracts provider names from workspace state (with resource-address fallback when state download fails), fetches the latest version of each `hashicorp/*` provider from the Terraform Registry, and surfaces known CVEs from OSV.dev per provider
- /providers slash command: per-workspace provider CVE and version report
- Known limitation: pinned module and provider versions are not available without access to the workspace's .tf files / .terraform.lock.hcl — the tools surface only the latest registry version and label every entry `check_recommended`. Provider CVE queries omit a version field, so the response surfaces every known CVE for the provider, framed as what an upgrade would address.
- Graceful degradation: version audit still returns groupings if OSV.dev is unreachable; module audit degrades to `latest_version: unavailable` per module on registry failures; provider audit falls back to resource-address provider extraction when state download fails and sets `cve_data_unavailable: true` when OSV is unreachable
- Full upgrade-effort scoring deferred to v1.5.1

## v1.6 — Plan Analyzer v2
- `how_to_reduce_risk` field per risk factor: concrete, actionable suggestions for making a High or Critical plan safer before applying
- Registry integration for module version context: surfaces whether a module version in the plan has known issues or a newer release
- Risk factor explanations written for the operator, not just the model — readable in the REPL without post-processing

## v1.7 — Observability and Metrics
- Usage analytics: sessions per workspace, tool call frequency, apply success rates
- Audit log visualization: searchable, filterable view of ~/.tfpilot/audit.log entries
- Agent call patterns: which tools are invoked together, which prompts lead to applies vs. read-only sessions
- Cost tracking per workspace: cumulative API spend attributed to workspace context
- Adoption metrics for internal rollout: active users per week, team coverage, first-use funnel
