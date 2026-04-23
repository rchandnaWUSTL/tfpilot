package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/rchandnaWUSTL/tfpilot/internal/provider/anthropic"
)

func TestStartupChecks_MissingHcptf(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	err := runStartupChecks()
	if err == nil {
		t.Fatal("expected error when hcptf not on PATH")
	}
	if !strings.Contains(err.Error(), "hcptf not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestAnthropicProvider_MissingAPIKey(t *testing.T) {
	// The Anthropic provider's Authenticate() now owns the ANTHROPIC_API_KEY
	// check that previously lived in runStartupChecks. Verify the error is
	// still user-friendly and actionable.
	t.Setenv("ANTHROPIC_API_KEY", "")
	p := anthropic.New(anthropic.Options{})
	err := p.Authenticate(context.Background())
	if err == nil {
		t.Fatal("expected error when ANTHROPIC_API_KEY missing")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStartupChecks_NoCredentials(t *testing.T) {
	if _, err := exec.LookPath("hcptf"); err != nil {
		t.Skip("hcptf not on PATH, skipping")
	}

	err := checkHCPTFCredentials()
	if err != nil && !strings.Contains(err.Error(), "credentials") && !strings.Contains(err.Error(), "hcptf") {
		t.Errorf("unexpected error format: %v", err)
	}
}
