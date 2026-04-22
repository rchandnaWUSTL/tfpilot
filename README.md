# Terraform Dev

AI-native terminal REPL for HCP Terraform.

![Demo](demo-v2.gif)

## Quick start

```bash
# 1. Install hcptf (HCP Terraform CLI that Terraform Dev shells out to)
git clone https://github.com/thrashr888/hcptf-cli.git
cd hcptf-cli && go install ./cmd/hcptf && cd ..

# 2. Build Terraform Dev
git clone https://github.com/rchandnaWUSTL/terraform-dev-terminal.git
cd terraform-dev-terminal
go build -o terraform-dev ./cmd/terraform-dev

# 3. Authenticate with your model provider (pick one — see "Authentication" below)
export ANTHROPIC_API_KEY=sk-ant-...   # option A: Anthropic
#   …or skip this and pass --auth=copilot on first run (option B: GitHub Copilot)

# 4. Run it
./terraform-dev --org=<your-org> --workspace=<your-workspace>
```

On first launch, Terraform Dev will run `hcptf login` for you if you don't already have HCP Terraform credentials — follow the browser prompt it opens.

---

## Prerequisites

- **Go 1.23+** — to build from source
- **`hcptf` on your `PATH`** — Terraform Dev is a thin agent layer on top of [`hcptf`](https://github.com/thrashr888/hcptf-cli); it will not start without it
- **A model provider credential** — either an Anthropic API key or a GitHub Copilot subscription

### Installing `hcptf`

Option 1 — build from source (recommended):

```bash
git clone https://github.com/thrashr888/hcptf-cli.git
cd hcptf-cli
go install ./cmd/hcptf       # drops hcptf into $GOPATH/bin (usually $HOME/go/bin)
hcptf whoami                 # should print your user; if "command not found",
                             # add $HOME/go/bin to $PATH
```

Option 2 — download a prebuilt binary from [hcptf-cli releases](https://github.com/thrashr888/hcptf-cli/releases) and place it on your `PATH`.

---

## Authentication

Terraform Dev needs two separate credentials: one for HCP Terraform (via `hcptf`) and one for the LLM provider.

### HCP Terraform

No configuration required. On startup, Terraform Dev runs `hcptf whoami` to check for cached credentials. If none are found, it transparently launches `hcptf login`, which opens a browser for OAuth. Once the browser step completes, the REPL proceeds.

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
   ./terraform-dev --org=<org> --workspace=<ws>
   ```

The default model is `claude-sonnet-4-6`, set in `~/.terraform-dev/config.yaml` (auto-created on first run).

#### Option B — GitHub Copilot

Requires an active Copilot subscription (Individual, Business, or Enterprise). No API key needed — Terraform Dev runs the OAuth device flow against GitHub.

1. Launch with the `--auth=copilot` flag:

   ```bash
   ./terraform-dev --auth=copilot --org=<org> --workspace=<ws>
   ```

2. On first run, Terraform Dev will prompt:
   - **Deployment type**: `1` for github.com (Individual / Business), `2` for self-hosted GitHub Enterprise (you'll be asked for your GHE domain).
   - Then print a GitHub verification URL and an 8-character user code. Open the URL, paste the code, and authorize the grant in your browser.

3. Credentials are cached at `~/.terraform-dev/copilot.json` (mode `0600`); subsequent launches skip the device flow. The short-lived Copilot chat token is refreshed transparently on HTTP 401.

Under `--auth=copilot`, the default model switches to `gpt-4o`. To use a different Copilot-hosted model (e.g. `claude-sonnet-4`), edit `model:` in `~/.terraform-dev/config.yaml`.

---

## Configuration

On first launch, `~/.terraform-dev/config.yaml` is created with defaults:

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
- **`✗ No HCP Terraform credentials found`** — run `hcptf login` manually and retry, or let Terraform Dev relaunch it for you.
- **Anthropic: `401 invalid x-api-key`** — `ANTHROPIC_API_KEY` is unset or wrong. `echo $ANTHROPIC_API_KEY` to verify.
- **Copilot: device flow loops `authorization_pending`** — you haven't entered the code in the browser yet; finish the GitHub grant and polling will complete.
- **Copilot: `404` on token exchange** — your GitHub account doesn't have an active Copilot subscription, or the cached token is stale. Delete `~/.terraform-dev/copilot.json` and rerun.

---

Built on [hcptf](https://github.com/thrashr888/hcptf-cli) by [@thrashr888](https://github.com/thrashr888).
