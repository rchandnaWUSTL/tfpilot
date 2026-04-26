package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/tfpilot/internal/config"
	"github.com/rchandnaWUSTL/tfpilot/internal/providerfactory"
	"github.com/rchandnaWUSTL/tfpilot/internal/repl"
)

var (
	bold = color.New(color.Bold)
	red  = color.New(color.FgRed)
)

func main() {
	org := flag.String("org", "", "HCP Terraform organization")
	workspace := flag.String("workspace", "", "HCP Terraform workspace")
	auth := flag.String("auth", "", "Auth backend: '' (default, use model_provider from config) or 'copilot'")
	apply := flag.Bool("apply", false, "Enable mutation mode (run create/apply/discard). Default is readonly.")
	watch := flag.Bool("watch", false, "Enable watch mode — scan org and surface suggestions proactively")
	mode := flag.String("mode", "suggest", "Watch mode: suggest (default) | report")
	flag.Parse()

	if err := runStartupChecks(); err != nil {
		fmt.Fprintln(os.Stderr)
		red.Fprintln(os.Stderr, "  "+err.Error())
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		red.Fprintf(os.Stderr, "  ✗ Failed to load config: %v\n", err)
		os.Exit(1)
	}

	authMode := providerfactory.AuthMode(*auth)
	prov, err := providerfactory.New(cfg, authMode)
	if err != nil {
		fmt.Fprintln(os.Stderr)
		red.Fprintf(os.Stderr, "  ✗ %v\n", err)
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	if err := prov.Authenticate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr)
		red.Fprintf(os.Stderr, "  %v\n", err)
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	cfg.Model = providerfactory.ModelFor(cfg, authMode)
	cfg.Readonly = !*apply
	cfg.Watch = *watch
	cfg.Mode = strings.ToLower(strings.TrimSpace(*mode))

	if cfg.Watch {
		if *org == "" {
			red.Fprintln(os.Stderr, "  ✗ --watch requires --org")
			os.Exit(1)
		}
		switch cfg.Mode {
		case "suggest":
			if cfg.Readonly {
				red.Fprintln(os.Stderr, "  ✗ Watch mode with --mode=suggest requires --apply")
				os.Exit(1)
			}
		case "report":
			// read-only, no --apply needed
		case "auto":
			red.Fprintln(os.Stderr, "  ✗ Auto mode not yet available. Use --mode=suggest.")
			os.Exit(1)
		default:
			red.Fprintf(os.Stderr, "  ✗ Unknown --mode=%s. Use suggest or report.\n", cfg.Mode)
			os.Exit(1)
		}

		if err := runWatchMode(context.Background(), cfg, *org); err != nil {
			red.Fprintf(os.Stderr, "  ✗ %v\n", err)
			os.Exit(1)
		}
		return
	}

	r := repl.New(cfg, prov, *org, *workspace)
	if err := r.Run(); err != nil {
		red.Fprintf(os.Stderr, "  ✗ %v\n", err)
		os.Exit(1)
	}
}

func runStartupChecks() error {
	if _, err := exec.LookPath("hcptf"); err != nil {
		return fmt.Errorf("✗ hcptf not found. Install it and ensure it's on your PATH.\n    https://github.com/thrashr888/hcptf-cli/releases")
	}

	if err := ensureHCPTFCredentials(); err != nil {
		return err
	}

	return nil
}

func ensureHCPTFCredentials() error {
	if err := checkHCPTFCredentials(); err == nil {
		return nil
	}

	// No credentials — run hcptf login inline with inherited stdio.
	fmt.Println()
	red.Println("  ✗ No HCP Terraform credentials found.")
	fmt.Println()
	fmt.Println("  Launching hcptf login...")
	fmt.Println()

	login := exec.Command("hcptf", "login")
	login.Stdin = os.Stdin
	login.Stdout = os.Stdout
	login.Stderr = os.Stderr
	if err := login.Run(); err != nil {
		return fmt.Errorf("✗ hcptf login failed. Try running it manually:\n    hcptf login")
	}

	fmt.Println()

	// Re-check after login.
	if err := checkHCPTFCredentials(); err != nil {
		return fmt.Errorf("✗ Still no valid credentials after login. Try: hcptf whoami")
	}

	return nil
}

func checkHCPTFCredentials() error {
	cmd := exec.Command("hcptf", "whoami", "-output=json")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("no credentials")
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("unexpected output from hcptf whoami")
	}

	return nil
}
