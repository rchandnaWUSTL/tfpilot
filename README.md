# tfpilot

AI-native terminal REPL for HCP Terraform.

![Demo](demo-mini.gif)

## Getting Started

### 1. Install `hcptf`

Binary install only — `go install` does not work because of a module path mismatch in the upstream repo.

```bash
curl -LO https://github.com/thrashr888/hcptf-cli/releases/download/v0.6.0/hcptf-cli_0.6.0_darwin_arm64.tar.gz
tar -xzf hcptf-cli_0.6.0_darwin_arm64.tar.gz
mkdir -p ~/bin && mv hcptf ~/bin/hcptf && chmod +x ~/bin/hcptf
echo 'export PATH="$HOME/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
hcptf version
```

> For other platforms see the full release list at [hcptf-cli releases](https://github.com/thrashr888/hcptf-cli/releases).

### 2. Authenticate with HCP Terraform

```bash
hcptf login
```

### 3. Clone and build tfpilot

```bash
git clone https://github.com/rchandnaWUSTL/terraform-dev-terminal.git
cd terraform-dev-terminal
go build -o tfpilot ./cmd/tfpilot
```

### 4. Run with Anthropic

```bash
export ANTHROPIC_API_KEY=your-key
./tfpilot --org=your-org --workspace=your-workspace
```

### 5. Run with GitHub Copilot

No API key needed — uses your existing Copilot license.

```bash
./tfpilot --auth=copilot --org=your-org --workspace=your-workspace
```

Follow the device flow prompt to authenticate.

---

## Prerequisites

- **Go 1.23+** — to build from source
- **`hcptf` on your `PATH`** — tfpilot is a thin agent layer on top of [`hcptf`](https://github.com/thrashr888/hcptf-cli); it will not start without it
- **A model provider credential** — either an Anthropic API key or a GitHub Copilot subscription

### Installing `hcptf`

Option 1 — install with Go (recommended):

```bash
go install github.com/thrashr888/hcptf-cli@latest
hcptf whoami                 # should print your user; if "command not found",
                             # add $HOME/go/bin to $PATH
```

Option 2 — download a prebuilt binary from [hcptf-cli releases](https://github.com/thrashr888/hcptf-cli/releases) and place it on your `PATH`.

---

## Authentication

tfpilot needs two separate credentials: one for HCP Terraform (via `hcptf`) and one for the LLM provider.

### HCP Terraform

No configuration required. On startup, tfpilot runs `hcptf whoami` to check for cached credentials. If none are found, it transparently launches `hcptf login`, which opens a browser for OAuth. Once the browser step completes, the REPL proceeds.

You can also run `hcptf login` manually ahead of time if you prefer.

### LLM provider — choose one

#### Option A — Anthropic (default)

1. Get an API key from [console.anthropic.com](https://console.anthropic.com/settings/keys).
2. Export it before launching:

   ```bash
   export ANTHROPIC_API_KEY=sk-ant-...
   ```

   Add it to your shell rc file (`~/.zshrc`, `~/.bashrc`) to persist across sessions.

3. Launch:

   ```bash
   ./tfpilot --org=<org> --workspace=<ws>
   ```

The default model is `claude-sonnet-4-6`, set in `~/.tfpilot/config.yaml` (auto-created on first run).

#### Option B — GitHub Copilot

Requires an active Copilot subscription (Individual, Business, or Enterprise). No API key needed — tfpilot runs the OAuth device flow against GitHub.

1. Launch with the `--auth=copilot` flag:

   ```bash
   ./tfpilot --auth=copilot --org=<org> --workspace=<ws>
   ```

2. On first run, tfpilot will prompt:
   - **Deployment type**: `1` for github.com (Individual / Business), `2` for self-hosted GitHub Enterprise (you'll be asked for your GHE domain).
   - Then print a GitHub verification URL and an 8-character user code. Open the URL, paste the code, and authorize the grant in your browser.

3. Credentials are cached at `~/.tfpilot/copilot.json` (mode `0600`); subsequent launches skip the device flow. The short-lived Copilot chat token is refreshed transparently on HTTP 401.

Under `--auth=copilot`, the default model switches to `gpt-4o`. To use a different Copilot-hosted model (e.g. `claude-sonnet-4`), edit `model:` in `~/.tfpilot/config.yaml`.

---

## Configuration

On first launch, `~/.tfpilot/config.yaml` is created with defaults:

```yaml
model: claude-sonnet-4-6
max_tokens: 16384
timeout_seconds: 10
readonly: true
model_provider: anthropic     # or "openai" for a self-hosted OpenAI-compatible endpoint
openai_base_url: ""           # optional, only used when model_provider=openai
```

The `--auth=copilot` flag overrides `model_provider` for that session without touching the file.

---

## Flags

| Flag | Purpose |
| --- | --- |
| `--org` | HCP Terraform organization name (required) |
| `--workspace` | HCP Terraform workspace name (required) |
| `--auth` | `copilot` to use GitHub Copilot; empty (default) to use `model_provider` from config |

---

## Troubleshooting

- **`✗ hcptf not found`** — `hcptf` isn't on your `PATH`. See [Installing `hcptf`](#installing-hcptf).
- **`✗ No HCP Terraform credentials found`** — run `hcptf login` manually and retry, or let tfpilot relaunch it for you.
- **Anthropic: `401 invalid x-api-key`** — `ANTHROPIC_API_KEY` is unset or wrong. `echo $ANTHROPIC_API_KEY` to verify.
- **Copilot: device flow loops `authorization_pending`** — you haven't entered the code in the browser yet; finish the GitHub grant and polling will complete.
- **Copilot: `404` on token exchange** — your GitHub account doesn't have an active Copilot subscription, or the cached token is stale. Delete `~/.tfpilot/copilot.json` and rerun.

---

Built on [hcptf](https://github.com/thrashr888/hcptf-cli) by [@thrashr888](https://github.com/thrashr888).
