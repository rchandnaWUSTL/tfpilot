# tfpilot — Demo Script

A 6-prompt walkthrough that shows the v0.6 feature surface end-to-end against a live HCP Terraform org. ~6 minutes total.

## Setup

```bash
./tfpilot --org=sarah-test-org --workspace=prod-k8s-apps --auth=copilot --apply
```

- `--auth=copilot` uses the user's existing GitHub Copilot license — no new API keys
- `--apply` unlocks mutation mode; remove it for the readonly-only portion of the demo
- Confirm the banner shows `mode: apply` and the model name (`gpt-4o` under Copilot, `claude-sonnet-4-6` under Anthropic)

## Act 1 — Discovery (2 prompts)

1. **"Describe the prod-k8s-apps workspace"**
   Shows: single tool call (`_hcp_tf_workspace_describe`), structured status-line response, resource count and types, last-run summary.
   Expected: ✓ Healthy status line, plain-prose summary, no run IDs leaked.

2. **"Any of my workspaces drifted this week?"**
   Shows: `_hcp_tf_drift_detect` called automatically, narrative summary of which workspaces show drift.
   Expected: per-workspace verdict with specific changed resources, or "No drift detected" with a next-action sentence.

## Act 2 — Comparison (2 prompts)

3. **"Compare prod-k8s-apps with staging-k8s-apps"**
   Shows: `_hcp_tf_workspace_diff` with parallel state fetch, structured resource-address diff.
   Expected: resources missing in each workspace, resources in both, per-workspace counts — all in prose, never as IDs.

4. **"What variables differ between those two workspaces?"**
   Shows: `_hcp_tf_variable_diff` returning keys only (values never exposed even when the CLI has them), sensitive flag preserved.
   Expected: keys unique to each workspace, keys in both, sensitive markers highlighted.

## Act 3 — Apply with approval (1 prompt)

5. **"Create a new run in prod-k8s-apps"**
   Shows: plan summary first, approval gate, type "no" to cancel safely.
   Expected: "Cancelled." printed, run discarded, audit log updated.

## Act 4 — Config generation (1 prompt)

6. **"Generate a null_resource called hello_world"**
   Shows: HCL generated, validated, written to disk, PR option offered.
   Expected: `./suggested_config.tf` on disk, `_hcp_tf_config_validate` returns `{valid: true, errors: []}`, agent offers Option A (keep local) / Option B (open PR via `_hcp_tf_pr_create`).

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
