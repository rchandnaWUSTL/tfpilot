# tfpilot

> Terminal co-pilot for HCP Terraform operations.

![Demo](demo-mini-v2.gif)

---

## What it does

Ask questions about your infra in plain English, from your terminal.

```
hcp-tf> why did the last run in prod-k8s-apps fail?
hcp-tf> is there any drift across my workspaces?
hcp-tf> how does prod compare to staging right now?
hcp-tf> what's the blast radius if I apply this?
hcp-tf> should I use a Stack or workspace for multi-region?
```

Read-only by default. Full audit log. Every action runs under your scoped HCP Terraform identity.

---

## Quickstart

Prerequisites:
- HCP Terraform account (authenticated via `hcptf login`)
- GitHub Copilot Enterprise **or** Anthropic API key
- [hcptf CLI](https://github.com/thrashr888/hcptf-cli/releases) on your PATH

Install hcptf (required — tfpilot uses it as its tool execution layer):
```bash
curl -LO https://github.com/thrashr888/hcptf-cli/releases/download/v0.6.0/hcptf-cli_0.6.0_darwin_arm64.tar.gz
tar -xzf hcptf-cli_0.6.0_darwin_arm64.tar.gz
mkdir -p ~/bin && mv hcptf ~/bin/hcptf && chmod +x ~/bin/hcptf
echo 'export PATH="$HOME/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
hcptf login
```

Build and run:
```bash
git clone https://github.com/rchandnaWUSTL/tfpilot.git
cd tfpilot
go build -o tfpilot ./cmd/tfpilot

# Already have GitHub Copilot Enterprise? Zero new API keys needed.
./tfpilot --org=my-org --workspace=prod-us-east-1 --auth=copilot

# Or with Anthropic API
export ANTHROPIC_API_KEY=your-key
./tfpilot --org=my-org --workspace=prod-us-east-1
```

---

## Features

- Workspace health, drift detection, run history
- Cross-environment diffs (resources + variables)
- Risk scoring and blast radius analysis before you apply
- Error diagnosis with categorized root cause and suggested fix
- Apply gates that scale with risk level (approval required, double confirmation for destructions)
- HCP Terraform Stacks support with Stack vs workspace guidance
- Config generation — describe intent, get valid HCL written to disk
- Workspace creation and resource provisioning from natural language — describe the workspace and resources, and tfpilot creates, uploads, and queues a run in one step
- Org-wide Terraform version audit with CVE checking — groups workspaces by version, flags known vulnerabilities (sourced from OSV.dev), scores upgrade complexity
- Per-workspace module audit — infers Terraform Registry modules from a workspace's resource addresses and surfaces the latest available versions from the public registry
- Per-workspace provider audit — extracts provider names from workspace state, surfaces latest versions from the public registry, and lists known CVEs from OSV.dev with upgrade notes
- Safe upgrade preview — generates a speculative plan against the local HCL with a bumped provider version constraint, then synthesizes the speculative plan's risk and blast radius, the CVE delta closed by the upgrade, and breaking changes parsed from upstream GitHub release notes into a concrete go / review / no-go recommendation. Available via the agent ("is it safe to upgrade aws to 5.91.0?") or the `/upgrade <provider> <version>` slash command. Requires `--apply` mode; the speculative run is discarded after analysis.
- Incident response — org-wide change timeline that fans out across every workspace and flags correlated changes, repeated failures, unexpected destructions, and off-hours activity. Pair it with enriched drift detection to identify the affected workspace and likely root cause, then use one-command run-based rollback (gated through plan analysis + apply confirmation) and an automated Markdown postmortem written to `~/.tfpilot/incidents/`. Demo flow: "something is wrong in prod, help me figure out what happened" → "is it safe to revert?" → "revert and write the report".

---

## How it works

tfpilot is a thin agent shell on top of [hcptf](https://github.com/thrashr888/hcptf-cli) by [@thrashr888](https://github.com/thrashr888). The agent (Claude or GPT-4o via Copilot) selects the right tools, calls them against your live HCP Terraform org, and synthesizes a plain-English response. Nothing is mocked — every response reflects real state.

---

## Requirements

- Go 1.23+ (to build from source)
- hcptf CLI on PATH
- HCP Terraform account
- GitHub Copilot Enterprise **or** Anthropic API key (`ANTHROPIC_API_KEY`)
