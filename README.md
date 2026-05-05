# tfpilot

> The AI that handles your Terraform on-call rotation.

![tfpilot demo](demo-mini-v2.gif)

Coming soon: ```bash
brew install tfpilot
```

---

## What it does

Ask your infrastructure questions in plain English.
Get answers, diagnoses, and fixes — without leaving your terminal.
hcp-tf> are we audit-ready? show me what's vulnerable before tomorrow's security review hcp-tf> something is wrong in prod, help me figure out what happened hcp-tf> is it safe to apply staging-api to prod-api? hcp-tf> what breaks if I change this workspace? hcp-tf> fix the rest


Read-only by default. Every action runs under your scoped HCP Terraform
identity. Full audit log at ~/.tfpilot/audit.log.

---

## Quickstart

**Step 1 — Install hcptf** (tfpilot's tool execution layer):

```bash
brew install hcptf
hcptf login
```

**Step 2 — Install tfpilot:**

```bash
brew install tfpilot
```

**Step 3 — Run it:**

```bash
# Already have GitHub Copilot Enterprise? Zero new API keys.
tfpilot --org=my-org --workspace=prod-us-east-1 --auth=copilot

# Or with Anthropic API
export ANTHROPIC_API_KEY=your-key
tfpilot --org=my-org --workspace=prod-us-east-1

# Or with OpenAI API
export OPENAI_API_KEY=your-key
tfpilot --org=my-org --workspace=prod-us-east-1 --auth=openai
```

---

## Build from source

```bash
curl -LO https://github.com/thrashr888/hcptf-cli/releases/download/v0.6.0/hcptf-cli_0.6.0_darwin_arm64.tar.gz
tar -xzf hcptf-cli_0.6.0_darwin_arm64.tar.gz
mkdir -p ~/bin && mv hcptf ~/bin/hcptf && chmod +x ~/bin/hcptf
echo 'export PATH="$HOME/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
hcptf login

git clone https://github.com/rchandnaWUSTL/tfpilot.git
cd tfpilot
go build -o tfpilot ./cmd/tfpilot
```

---

## What it can do

- **Compliance & security** — org-wide CVE scan across all workspaces,
  prioritized remediation queue, one-command batch upgrades with approval
  gates, CISO-shareable compliance report generated automatically
- **Safe upgrade preview** — speculative plan against bumped provider
  version, CVE delta, breaking changes from GitHub release notes,
  concrete go / review / no-go recommendation
- **Incident response** — org-wide change timeline, correlated failure
  detection, drift-based root cause analysis, one-command rollback,
  automated postmortem written to disk
- **Pre-deploy safety** — cross-environment diffs, blast radius, risk
  scoring, policy checks, cost delta — before you apply anything
- **Dependency mapping** — what breaks if I change this workspace?
- **Workspace provisioning** — describe what you want in plain English,
  tfpilot creates the workspace, uploads config, and queues the run
- **Watch mode** — boots silently, scans for vulnerabilities, surfaces
  prioritized suggestions, executes with a single y/n approval

---

## Requirements

- HCP Terraform account
- GitHub Copilot Enterprise, Anthropic API key, **or** OpenAI API key
