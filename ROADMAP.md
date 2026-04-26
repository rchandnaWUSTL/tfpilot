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
- _hcp_tf_provider_audit tool: extracts provider names from workspace state (with resource-address fallback when state download fails), probes the most recent plan export's required_providers block to recover per-provider version constraints, fetches the latest version of each `hashicorp/*` provider from the Terraform Registry, and queries OSV.dev for every known CVE per provider. Each provider entry returns three slices — `all_cves`, `currently_affected`, and `upgrading_fixes` — partitioned by comparing the pinned version to each CVE's `fixed_in` field. Exact constraints (e.g. `4.9.0`) populate `pinned_version`; range constraints (`~> 4.45.0`, etc.) leave `pinned_version: unknown` and surface the raw constraint instead. The envelope's `pinned_version_source` is `planexport` when constraints were discovered, `unknown` otherwise.
- /providers slash command: per-workspace provider CVE and version report with separate sections for All known CVEs, Currently affected, and Fixed by upgrading
- Module audit's known limitation persists: pinned module versions are not available without access to the workspace's .tf files. Provider audit recovers constraints via plan export, so range constraints are honestly reported as `pinned_version: unknown` rather than mislabeled as exact versions.
- Graceful degradation: version audit still returns groupings if OSV.dev is unreachable; module audit degrades to `latest_version: unavailable` per module on registry failures; provider audit falls back to resource-address provider extraction when state download fails, degrades `pinned_version_source` to `unknown` when plan export is unavailable, and sets `cve_data_unavailable: true` when OSV is unreachable. Plan exports are best-effort cleaned up after the audit; a stale export on the next run is recovered via the TFC JSON API.
- Full upgrade-effort scoring deferred to v1.5.1

## v1.6 — Safe Upgrade Preview (In Progress)
- _hcp_tf_upgrade_preview tool: generates a what-if speculative plan by staging the workspace's local HCL into a tempdir, rewriting the named provider's version constraint to `= <target_version>`, uploading the result as a speculative configuration version, and waiting for the auto-queued plan-only run to reach a terminal state. Mutating — requires --apply, REPL-gated, speculative run is discarded after analysis.
- Feeds the speculative run through _hcp_tf_plan_analyze for the same risk_level / blast_radius / risk_factors output a normal plan would produce — the upgrade is judged against the workspace's actual resources, not a generic checklist.
- Cross-references _hcp_tf_provider_audit's `upgrading_fixes` set and partitions it into `cves_fixed` for the chosen target_version: a CVE is counted as fixed only when its `fixed_in` is at or below the target.
- Fetches GitHub release notes between the pinned and target versions from `https://api.github.com/repos/hashicorp/terraform-provider-<name>/releases`. Parses explicit `BREAKING CHANGES:` sections first; falls back to a keyword scan (breaking, removed, deprecated, no longer, must now). Honors GITHUB_TOKEN for higher API rate limits; degrades `breaking_changes_source` to `unavailable` or `rate_limited` rather than failing the tool.
- Synthesizes a single `recommendation` (go|review|no_go) from the four signals: Critical risk → no_go; breaking change that touches a resource type in the plan's blast radius → no_go; High risk or any breaking changes present or unknown pinned version → review; Low/Medium with no breaking changes and at least one CVE closed → go. Returns `recommendation_reason` in plain English citing which signal drove the call.
- /upgrade <provider> <version> slash command: bypasses the agent path, calls the tool directly, and pretty-prints risk + blast radius + CVE fix list + breaking changes + recommendation using the existing color helpers. Refuses in readonly mode with a clear "use --apply" message.
- System prompt rule: when the user asks "is it safe to upgrade …", the agent calls _hcp_tf_upgrade_preview; if no target_version is given, it chains through _hcp_tf_provider_audit first to discover latest_version. Generic upgrade advice is explicitly forbidden.
- Resolves Issue 3 from the post-v1.5 audit: "is it safe to upgrade?" returns a real risk score, blast radius, CVE diff, and breaking-changes summary grounded in the user's workspace, never generic advice.
- Constraint: the workspace's HCL must be in the tool's `config_path` (defaults to cwd). VCS-only workspaces and configurations the user does not have locally surface an `unsupported_operation` error explaining the requirement.

## v1.7 — Plan Analyzer v2
- `how_to_reduce_risk` field per risk factor: concrete, actionable suggestions for making a High or Critical plan safer before applying
- Registry integration for module version context: surfaces whether a module version in the plan has known issues or a newer release
- Risk factor explanations written for the operator, not just the model — readable in the REPL without post-processing

## v1.8 — Observability and Metrics
- Usage analytics: sessions per workspace, tool call frequency, apply success rates
- Audit log visualization: searchable, filterable view of ~/.tfpilot/audit.log entries
- Agent call patterns: which tools are invoked together, which prompts lead to applies vs. read-only sessions
- Cost tracking per workspace: cumulative API spend attributed to workspace context
- Adoption metrics for internal rollout: active users per week, team coverage, first-use funnel

## v2.0 — Incident Response (In Progress)
- _hcp_tf_org_timeline tool: fans out across every workspace with resources>0, returns merged run history sorted newest-first within a configurable lookback window (default 24h). Each entry carries workspace, run_id, status, message, created_at + relative time, triggered_by source, and best-effort plan counts (additions/changes/destructions). Detects four anomaly classes: multiple_changes_in_window (2+ workspaces within 30 minutes), repeated_failure (same workspace errored 2+ times), unexpected_destruction (any run with destructions>0), and off_hours_change (UTC hour outside 06:00-22:00). Read-only.
- _hcp_tf_drift_detect enrichment: structured drifted_resources[{address, provider, change_type ∈ modified|deleted|added|replaced|changed}], assessment_status (ok|drifted|error), summary string, and last_assessed_human relative time. Drift addresses are also kept as drifted_addresses[] for back-compat with existing consumers. Endpoint (/api/v2/workspaces/<id>/current-assessment-result) was already corrected in v1.9.
- _hcp_tf_rollback tool: run-based rollback. Picks the most recent applied run other than the current one (or an explicit run_id), fetches that run's configuration-version-id from the JSON:API, and POSTs a new run against that configuration with auto-apply=false. Mutating — requires --apply. The new run goes through the existing approveMutation gate at creation, then through _hcp_tf_plan_analyze + applyGate before any infrastructure mutation.
- _hcp_tf_incident_summary tool: pure local transformation that takes the JSON outputs of a prior _hcp_tf_org_timeline and _hcp_tf_drift_detect call (plus optionally a rollback run id) and synthesizes a Markdown postmortem written to ~/.tfpilot/incidents/<YYYY-MM-DD>-<workspace>.md. Sections: Summary, Timeline table, Root Cause (inferred from drifted resource types — security_group / network_acl / iam_ / _role / _policy flagged as likely manual changes), Impact, Resolution, and Action Items. Read-only with respect to HCP Terraform; writes to local disk only.
- Cross-workspace correlation rule: the agent reasons about related changes across workspaces in the same 30-minute window and surfaces them as potentially related during incident triage.
- 3-prompt incident demo flow: "something is wrong in prod, help me figure out what happened" (timeline + correlation + drift) → "is it safe to revert?" (rollback creates plan-only run, plan_analyze surfaces blast radius) → "revert it and write the report" (apply gated rollback + Markdown postmortem written to disk).
