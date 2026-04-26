# v1.6 — Safe Upgrade Preview

## Context

Post-v1.5 audit Issue 3: when a user asks "is it safe to upgrade the AWS provider to 5.91.0?", tfpilot must return a real risk score, blast radius, CVE delta, and breaking-changes summary grounded in the user's workspace — never generic upgrade advice.

v1.5 already ships `_hcp_tf_provider_audit` (extracts pinned + latest version, lists CVEs from OSV.dev, partitions into `currently_affected` vs `upgrading_fixes`) and `_hcp_tf_plan_analyze` (risk score + blast radius for any run). v1.6 stitches them together by generating a speculative plan against a bumped provider version and synthesizing the four signals (plan risk, CVE delta, breaking changes, version-source confidence) into a single go / review / no-go recommendation.

Test workspace: `learn-terraform-data-sources-vpc` in `sarah-test-org`. The user must launch tfpilot from a directory containing the workspace's HCL — the constraint surfaces because `hcptf` does not expose a `configversion download` subcommand, so the tool cannot fetch the workspace's existing config from HCP Terraform alone.

## Goal

A single tool, a single slash command, and a single agent-routing rule that together replace generic upgrade advice with a concrete recommendation backed by:

1. **Plan risk** — the speculative plan run through `_hcp_tf_plan_analyze`
2. **CVE delta** — `upgrading_fixes` from `_hcp_tf_provider_audit`, filtered to those whose `fixed_in` is at or below the target
3. **Breaking changes** — parsed from upstream GitHub release notes (`https://api.github.com/repos/hashicorp/terraform-provider-<name>/releases`)
4. **Version-source confidence** — the audit's `pinned_version_source` field

## Implementation

### 1. New tool in internal/tools/upgrade_preview.go

`_hcp_tf_upgrade_preview(org, workspace, provider, target_version, [config_path])`

Flow:
1. Resolve `config_path` (default cwd); error early if no `.tf` files.
2. Call `providerAuditCall` in-process for `pinned_version`, `pinned_version_source`, and `upgrading_fixes`. Verify the named provider exists in the workspace; otherwise return `invalid_tool`.
3. Stage all top-level `.tf` files into a tempdir.
4. `mutateProviderVersion`: regex-rewrite the provider's version constraint in two HCL forms — `terraform { required_providers { <name> = { version = "..." } } }` and the legacy `provider "<name>" { version = "..." }`. Error if neither matches.
5. `hcptf configversion create -speculative -org=X -workspace=Y -output=json` — returns cv ID + one-shot upload URL.
6. PUT the tar.gz via the existing `putArchivist` helper.
7. `waitForSpeculativeRun`: poll `hcptf run list` then `hcptf run show` per ID, match `ConfigurationVersionID` to the cv ID, then wait for the auto-queued speculative run to reach `planned_and_finished` (or `errored`/`canceled`/`discarded`). 5-minute internal deadline.
8. `planAnalyzeCall(run_id)` in-process for risk + blast radius.
9. `fetchProviderReleaseNotes(provider, fromVersion, toVersion)` — GitHub Releases API, honors `GITHUB_TOKEN`, parses `BREAKING CHANGES:` sections first then keyword-scans the rest. Degrades to `breaking_changes_source: unavailable` or `rate_limited` rather than failing the tool.
10. `filterCVEsFixedBy(upgradingFixes, targetVersion)` — keep CVEs whose `fixed_in <= target_version` (or that have no `fixed_in`).
11. `synthesizeUpgradeRecommendation`: critical risk → no_go; breaking change touching a resource type in the plan's blast radius → no_go; high risk OR breaking changes present OR unknown pinned version → review; otherwise go (when at least one CVE closes) or review (when nothing is gained).
12. Discard the speculative run via deferred `hcptf run discard` (best-effort).

Tool registration in `internal/tools/tools.go`:
- `MutatingTools["_hcp_tf_upgrade_preview"] = true` (creates a configversion even though the run never applies)
- Dispatch entry in `callDispatch` after `_hcp_tf_workspace_populate`
- Tool definition in `Definitions()` before `_hcp_tf_config_validate`

### 2. /upgrade slash command in internal/repl/repl.go

`/upgrade <provider> <version>` — refuses in readonly mode, calls `r.approveMutation` to mirror the agent-path approval gate, then calls `tools.Call` directly. Pretty-prints with the existing color palette:
- Header: `tfPurple` "Upgrade Preview — workspace: provider → target_version"
- Risk Level: `waypointTeal` (Low/Medium), `vaultYellow` (High), `boundaryPink` (Critical)
- Blast Radius: dim white, single line with destructions/additions/changes
- CVEs fixed: per-line color by severity, indented
- Breaking changes: `vaultYellow` warning markers, indented; explicit "none detected" / "GitHub release notes unavailable" lines when empty
- Recommendation: full-line color matching go/review/no_go, with `recommendation_reason`

Add help text in `printHelp`.

### 3. System prompt update in internal/agent/agent.go

Add one rule after the existing provider-audit rule:
- When asked if it is safe to upgrade a provider, call `_hcp_tf_upgrade_preview` with org, workspace, provider (short name), and target_version.
- Surface risk_level, blast_radius, cves_fixed (count + most severe), breaking_changes (first 2-3 lines), recommendation + reason.
- Never return generic upgrade advice. If the tool errors, name the error_code and stop.
- If target_version is missing, chain through `_hcp_tf_provider_audit` first to discover `latest_version`.
- The tool requires `--apply` mode and the workspace's local HCL in cwd; surface `unsupported_operation` errors plainly.

### 4. ROADMAP.md / README.md / prd.md

- ROADMAP: replace existing v1.6 placeholder (Plan Analyzer v2) with the new v1.6 — Safe Upgrade Preview entry; renumber Plan Analyzer v2 → v1.7; renumber Observability and Metrics → v1.8.
- README: append Safe upgrade preview bullet to the Features list.
- prd.md: add `_hcp_tf_upgrade_preview` row to the mutating-tools table.

## Testing

```bash
go build -o tfpilot ./cmd/tfpilot
go test ./...
```

Unit tests in `internal/tools/upgrade_preview_test.go` cover:
- Recommendation synthesis across the eight key cases (Critical → no_go, breaking-change intersection → no_go, High → review, breaking-change present → review, unknown pinned → review, low+CVEs → go, rate-limit-only line treated as no breaking changes, no-CVEs → review)
- `filterCVEsFixedBy` semver filtering (target 5.91.0 keeps fixes ≤ 5.91.0 and CVEs without fixed_in; drops fixes for 6.0.0)
- `mutateProviderVersion` against both HCL forms (`required_providers` and legacy `provider "<name>"`)
- `extractBreakingLines` against the AWS provider's `BREAKING CHANGES:` section format

Live REPL — agent path:
```
$ cd ~/path/to/learn-terraform-data-sources-vpc   # cwd needs the HCL
$ ./tfpilot --org=sarah-test-org --workspace=learn-terraform-data-sources-vpc --auth=copilot --apply
> is it safe to upgrade the AWS provider to 5.91.0?
```
Expected: agent calls `_hcp_tf_upgrade_preview`, REPL approval gate prompts, speculative run runs, agent surfaces risk_level + blast_radius + CVE count + breaking_changes + recommendation in plain prose. Must NOT be generic upgrade advice.

Live REPL — slash-command path:
```
> /upgrade aws 5.91.0
```
Expected: same backing tool call, pretty-printed output as in section 2 above.

Cleanup verification:
```
$ hcptf run list -workspace=learn-terraform-data-sources-vpc -org=sarah-test-org -output=json | head
```
The most recent run's status should be `discarded`.

Audit log:
```
$ tail -3 ~/.tfpilot/audit.log | python3 -m json.tool
```
One entry per tool invocation: `_hcp_tf_upgrade_preview` plus the deferred `_hcp_tf_run_discard`.

## Acceptance criteria

- "is it safe to upgrade the AWS provider to 5.91.0?" returns a real risk score, blast radius, CVE diff, and breaking-changes summary — never generic advice.
- `/upgrade aws 5.91.0` produces the same output via slash-command path.
- Speculative run is discarded after analysis (verified via `hcptf run list`).
- `go test ./...` is green.
- Commit: `feat: v1.6 safe upgrade preview — speculative plan + CVE diff + breaking changes`.
- Push to main; close all four subtasks before closing the parent.
