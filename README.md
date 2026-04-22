# Terraform Dev

AI-native terminal REPL for HCP Terraform.

![Demo](demo-v2.gif)

## Quick start

Prerequisites:

- Go 1.23+
- [`hcptf`](https://github.com/thrashr888/hcptf-cli/releases) on your `PATH`:

  ```bash
  curl -LO https://github.com/thrashr888/hcptf-cli/releases/download/v0.6.0/hcptf-cli_0.6.0_darwin_arm64.tar.gz
  tar -xzf hcptf-cli_0.6.0_darwin_arm64.tar.gz
  mkdir -p ~/bin && mv hcptf ~/bin/hcptf && chmod +x ~/bin/hcptf
  ```

  Then ensure `~/bin` is on your PATH. For other platforms, see https://github.com/thrashr888/hcptf-cli/releases.

  Note: `go install` does not work due to a module path mismatch in the hcptf repo.
- `ANTHROPIC_API_KEY` set in your environment

Install:

```bash
git clone https://github.com/rchandnaWUSTL/terraform-dev-terminal.git
cd terraform-dev-terminal
go build -o terraform-dev ./cmd/terraform-dev
./terraform-dev --org=<your-org> --workspace=<your-workspace>
```

---

Built on [hcptf](https://github.com/thrashr888/hcptf-cli) by [@thrashr888](https://github.com/thrashr888).
