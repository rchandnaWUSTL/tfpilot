# tfpilot — Demo Script

An 8-prompt walkthrough that shows the v0.11 feature surface end-to-end against a live HCP Terraform org. ~8 minutes total.

## Setup

```bash
./tfpilot --org=sarah-test-org --workspace=prod-api --auth=copilot --apply
```

- `--auth=copilot` uses the user's existing GitHub Copilot license — no new API keys
- `--apply` unlocks mutation mode; remove it for the readonly-only portion of the demo
- Confirm the banner shows `mode: apply` and the model name (`gpt-4o` under Copilot, `claude-sonnet-4-6` under Anthropic)
- `prod-api` and `staging-api` live in the `zzryan` project and were themselves created by tfpilot — see `ops/now/v11-workspace-lifecycle-demo-log.md`

## Act 0 — Setup (2 prompts, only if starting from a clean org)

Skip this act if `prod-api` and `staging-api` already exist.

1. **"Create a workspace called prod-api in the zzryan project"**
   Shows: approval gate, `_hcp_tf_workspace_create` called, workspace URL surfaced.
   Expected: `prod-api` appears under the zzryan project in the HCP Terraform UI.

2. **"Generate a VPC, public subnet, EC2 instance, and security group as null_resources for prod-api, and apply it"**
   Shows: HCL generated, `_hcp_tf_config_validate` passes, "Apply this config directly to prod-api?" prompt, `_hcp_tf_workspace_populate` fires, run triggered.
   Expected: configuration version uploaded, run reaches `planned` / `cost_estimated`, ready for apply.

## Act 1 — Discovery (2 prompts)

3. **"Describe the prod-api workspace"**
   Shows: single tool call (`_hcp_tf_workspace_describe`), structured status-line response, resource count and types, last-run summary.
   Expected: ✓ Healthy status line, plain-prose summary, no run IDs leaked.

4. **"Any of my workspaces drifted this week?"**
   Shows: `_hcp_tf_drift_detect` called automatically, narrative summary of which workspaces show drift.
   Expected: per-workspace verdict with specific changed resources, or "No drift detected" with a next-action sentence.

## Act 2 — Comparison (2 prompts)

5. **"Compare prod-api with staging-api"**
   Shows: `_hcp_tf_workspace_diff` with parallel state fetch, structured resource-address diff.
   Expected: prod-api has EC2 instance + security group that staging-api is missing, both share the VPC and public subnet — all in prose, never as IDs.

6. **"What variables differ between those two workspaces?"**
   Shows: `_hcp_tf_variable_diff` returning keys only (values never exposed even when the CLI has them), sensitive flag preserved.
   Expected: keys unique to each workspace, keys in both, sensitive markers highlighted.

## Act 3 — Apply with plan analysis (1 prompt)

7. **"Create a new run in prod-api and analyze the plan before applying"**
   Shows: `_hcp_tf_run_create` → `_hcp_tf_plan_analyze` fires automatically, risk scoring rendered (Low/Medium/High/Critical) with blast radius, apply gate scales the confirmation prompt to the risk level. Type "no" to cancel safely.
   Expected: plan-analyzer verdict surfaces before the confirmation prompt; "Cancelled." printed on no, run auto-discarded, audit log updated.

## Act 4 — Config generation (1 prompt)

8. **"Generate a null_resource called hello_world"**
   Shows: HCL generated, validated, written to disk, direct-apply + PR options offered.
   Expected: `./suggested_config.tf` on disk, `_hcp_tf_config_validate` returns `{valid: true, errors: []}`, agent offers Option A (keep local) / Option B (apply directly to prod-api via `_hcp_tf_workspace_populate`) / Option C (open PR via `_hcp_tf_pr_create`).

## Key talking points

- Zero new API keys with `--auth=copilot` (uses existing Copilot license)
- Read-only by default — nothing changes without explicit approval
- Every action logged to `~/.tfpilot/audit.log`
- HashiCorp brand colors throughout
- Runs on gpt-4o via Copilot or claude-sonnet-4-6 via Anthropic

## Closing

```bash
/exit
cat ~/.tfpilot/audit.log | tail -10
```

Shows the JSON-line audit trail of every tool call — including the cancelled run_create with `error_code: user_cancelled` — as the closing beat.
