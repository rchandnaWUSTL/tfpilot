package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/terraform-dev/internal/config"
	"github.com/rchandnaWUSTL/terraform-dev/internal/repl"
)

var (
	bold = color.New(color.Bold)
	red  = color.New(color.FgRed)
)

func main() {
	org := flag.String("org", "", "HCP Terraform organization")
	workspace := flag.String("workspace", "", "HCP Terraform workspace")
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

	r := repl.New(cfg, *org, *workspace)
	if err := r.Run(); err != nil {
		red.Fprintf(os.Stderr, "  ✗ %v\n", err)
		os.Exit(1)
	}
}

func runStartupChecks() error {
	if _, err := exec.LookPath("hcptf"); err != nil {
		return fmt.Errorf("✗ hcptf not found. Install it and ensure it's on your PATH.\n    https://github.com/thrashr888/hcptf-cli/releases")
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("✗ ANTHROPIC_API_KEY not found in environment.\n    export ANTHROPIC_API_KEY=your-key")
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
