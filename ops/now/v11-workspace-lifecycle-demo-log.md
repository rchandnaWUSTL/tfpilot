# v0.11 — Workspace Lifecycle demo-environment log

Captured: 2026-04-24
Org: sarah-test-org
Project: zzryan (`prj-aWKHwXFckX3nRtTf`)
Operator: roshan@chandna.com

This log is the durable artifact of the v0.11 demo-environment validation
step (aiki subtask `tsvmppw`). It records the actual terminal output of
creating and populating `prod-api` and `staging-api` end-to-end using the
new `_hcp_tf_workspace_create` and `_hcp_tf_workspace_populate` tools,
followed by a `_hcp_tf_workspace_diff` verifying resource shape differences.

## Pre-flight

- Pre-check: neither `prod-api` nor `staging-api` existed in
  `sarah-test-org` before the run (confirmed via `hcptf workspace list`).
- A first driver run hit an hcptf quirk — `hcptf configversion upload`
  tries to re-fetch the archivist UploadURL, but the URL is one-shot
  from the initial `configversion create` response. After fixing
  `workspacePopulateCall` to capture the UploadURL and PUT the tar.gz
  directly, the two stale workspaces from the first attempt were
  force-deleted (`hcptf workspace delete -force`) and the driver was
  re-run clean.

## Driver output

```
=== Creating workspace prod-api ===
  [workspace_create] OK (931ms)
  {
    "description": "tfpilot v0.11 demo workspace (prod-api)",
    "name": "prod-api",
    "org": "sarah-test-org",
    "project": "zzryan",
    "project_id": "prj-aWKHwXFckX3nRtTf",
    "terraform_version": "1.14.9",
    "url": "https://app.terraform.io/app/sarah-test-org/workspaces/prod-api",
    "workspace_id": "ws-MtRyyGpcZju2mB1X"
  }

=== Populating prod-api with HCL ===
  [workspace_populate] OK (10.646s)
  {
    "configuration_version_id": "cv-uLX3dLVvosXXq4PU",
    "message": "Run triggered. Use _hcp_tf_runs_list_recent to check status.",
    "org": "sarah-test-org",
    "run_id": "run-eZDeAGnGuoKtiNV6",
    "status": "pending",
    "terraform_init": "ok",
    "workspace": "prod-api"
  }

=== Creating workspace staging-api ===
  [workspace_create] OK (1.255s)
  {
    "description": "tfpilot v0.11 demo workspace (staging-api)",
    "name": "staging-api",
    "org": "sarah-test-org",
    "project": "zzryan",
    "project_id": "prj-aWKHwXFckX3nRtTf",
    "terraform_version": "1.14.9",
    "url": "https://app.terraform.io/app/sarah-test-org/workspaces/staging-api",
    "workspace_id": "ws-NhmVUhgn2SpwTVMU"
  }

=== Populating staging-api with HCL ===
  [workspace_populate] OK (8.937s)
  {
    "configuration_version_id": "cv-GvGjM5Kz7aXh2TKN",
    "message": "Run triggered. Use _hcp_tf_runs_list_recent to check status.",
    "org": "sarah-test-org",
    "run_id": "run-4GpzVEXSkioLhAjb",
    "status": "pending",
    "terraform_init": "ok",
    "workspace": "staging-api"
  }
```

Note: `configversion create` has `-auto-queue-runs` enabled by default, so
the HCP Terraform API queues a speculative-like run on config upload in
addition to the explicit `hcptf run create` the tool fires. The
auto-queued runs (`run-GLXCHVPNjpdw1Mib` in prod, `run-PjU8wHCNC3mrCKzQ`
in staging) planned, reached `cost_estimated`, and were approved via
`hcptf run apply` with comment "tfpilot v0.11 demo provisioning". Both
reached `applied` status. The explicitly-created runs remained pending
behind the auto-queued ones — this is the expected HCP Terraform
serialization behavior for a single workspace and does not indicate a
bug in the new tool. A follow-up refinement is to make
`workspace_populate` either skip the explicit `run create` when
`-auto-queue-runs` is on, or flip `-auto-queue-runs=false` so only one
run is produced; tracked as a post-v0.11 nicety.

## Apply confirmation

```
Run run-GLXCHVPNjpdw1Mib has been approved and is applying
Run run-PjU8wHCNC3mrCKzQ has been approved and is applying
```

After ~20 seconds:

```
prod-api: applied
staging-api: applied
```

## Diff verification

```
{
  "missing_in_a": [],
  "missing_in_b": [
    "null_resource.ec2_instance",
    "null_resource.security_group"
  ],
  "present_in_both": [
    "null_resource.public_subnet",
    "null_resource.vpc"
  ],
  "workspace_a_resource_count": 4,
  "workspace_b_resource_count": 2
}
```

This is the expected shape:
- prod-api contains VPC + public subnet + EC2 instance + security group (4 resources)
- staging-api contains VPC + public subnet only (2 resources)
- Diff correctly names ec2_instance and security_group as missing in staging

## Human-verification gate

The output above is evidence, not proof. Before closing the parent aiki
task (`lmvwptz`), the human operator opens the HCP Terraform UI at
https://app.terraform.io/app/sarah-test-org/workspaces and visually
confirms:

- `prod-api` exists in the `zzryan` project
- `staging-api` exists in the `zzryan` project
- `prod-api` latest run reached `applied` with 4 null_resources
- `staging-api` latest run reached `applied` with 2 null_resources

## Temporary drivers

Two temporary cmd directories were used to exercise the new tool
functions outside the REPL (driving `tools.Call` directly from Go):

- `cmd/tfpilot-demo-setup/main.go` — creates both workspaces and
  populates them
- `cmd/tfpilot-demo-diff/main.go` — runs `_hcp_tf_workspace_diff`
  across them

Both directories are removed before the v0.11 commit so only the real
tool code ships. The log above is the authoritative record of what they
did.
