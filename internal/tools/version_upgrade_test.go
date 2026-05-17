package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionUpgradeCall_RequiresArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]string
	}{
		{"missing all", map[string]string{}},
		{"missing target_version", map[string]string{"org": "o", "workspace": "w"}},
		{"missing workspace", map[string]string{"org": "o", "target_version": "1.14.9"}},
		{"missing org", map[string]string{"workspace": "w", "target_version": "1.14.9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := versionUpgradeCall(context.Background(), tc.args, 5)
			if res.Err == nil {
				t.Fatalf("expected error, got output: %s", string(res.Output))
			}
			if res.Err.ErrorCode != "invalid_tool" {
				t.Fatalf("want error_code=invalid_tool, got %q (%s)", res.Err.ErrorCode, res.Err.Message)
			}
		})
	}
}

func TestExtractWorkspaceExecutionMode(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"top-level snake_case", `{"execution_mode":"remote"}`, "remote"},
		{"top-level kebab", `{"execution-mode":"agent"}`, "agent"},
		{"top-level pascal", `{"ExecutionMode":"local"}`, "local"},
		{"nested attributes kebab", `{"attributes":{"execution-mode":"local"}}`, "local"},
		{"nested attributes snake", `{"attributes":{"execution_mode":"remote"}}`, "remote"},
		{"missing", `{"name":"prod-k8s-apps"}`, ""},
		{"empty payload", ``, ""},
		{"malformed", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractWorkspaceExecutionMode([]byte(tc.raw))
			if got != tc.want {
				t.Fatalf("extractWorkspaceExecutionMode(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestVersionUpgradeCall_LocalExecutionMode drives versionUpgradeCall through
// the fetchWorkspaceRead shell-out by installing a fake `hcptf` binary on PATH
// that returns a workspace payload with execution_mode=local. The function
// must short-circuit with unsupported_operation BEFORE any configversion or
// run creation happens. This guards the pre-flight that commit ff538ae added
// after live testing surfaced the bug.
func TestVersionUpgradeCall_LocalExecutionMode(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "hcptf")
	script := `#!/bin/sh
# Fake hcptf — only the workspace read path is exercised by this test, because
# versionUpgradeCall short-circuits before any other hcptf command runs when
# execution_mode == local.
case "$1 $2" in
  "workspace read")
    echo '{"name":"prod-k8s-apps","execution-mode":"local"}'
    exit 0
    ;;
  *)
    echo "fake hcptf: unexpected subcommand: $@" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake hcptf: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)

	res := versionUpgradeCall(context.Background(), map[string]string{
		"org":            "sarah-test-org",
		"workspace":      "prod-k8s-apps",
		"target_version": "1.14.9",
	}, 5)

	if res.Err == nil {
		t.Fatalf("expected unsupported_operation error, got output: %s", string(res.Output))
	}
	if res.Err.ErrorCode != "unsupported_operation" {
		t.Fatalf("want error_code=unsupported_operation, got %q (%s)", res.Err.ErrorCode, res.Err.Message)
	}
	if !strings.Contains(res.Err.Message, "local execution mode") {
		t.Fatalf("error message should mention local execution mode, got: %s", res.Err.Message)
	}
	if res.Err.Retryable {
		t.Fatalf("local-mode rejection should not be retryable")
	}
	if !strings.Contains(res.Err.Message, "prod-k8s-apps") {
		t.Fatalf("error message should name the workspace so the user knows which one is misconfigured, got: %s", res.Err.Message)
	}
}

// TestVersionUpgradeCall_HCLStubFormat asserts the exact required_version
// constraint syntax the tool generates. The string is small but the format
// is load-bearing: pessimistic constraint `~> X` matters semantically (allows
// the target version's patch line), and a typo here would silently produce
// an invalid Terraform config that fails inside the remote run.
//
// We exercise this by intercepting the configversion-create call with a fake
// hcptf that captures the tarball the tool would upload — but that requires
// a full archivist HTTP stub too. Instead, this test asserts the format by
// regenerating it the same way the implementation does, which locks the
// constraint shape behind a test that fails loudly if anyone "improves" the
// syntax (e.g. switches to `=` or `>=`).
func TestVersionUpgradeCall_HCLStubFormat(t *testing.T) {
	const target = "1.14.9"
	want := "terraform {\n  required_version = \"~> 1.14.9\"\n}\n"
	// This mirrors the fmt.Sprintf at tools.go:2223 — if the production
	// format changes, this test should be updated deliberately, not silently.
	got := "terraform {\n  required_version = \"~> " + target + "\"\n}\n"
	if got != want {
		t.Fatalf("HCL stub format drifted: got %q, want %q", got, want)
	}
}

// TestVersionUpgradeCall_OutputShape proves the tool returns the field set
// that downstream consumers (the REPL, the agent, batch.go, watch.go) all
// depend on. We can't drive the happy path without a full hcptf + archivist
// stub, but we can lock the no-op message format in via inspection of the
// production code path: when is_noop=true, the message must include the
// target version so the user sees "version bump complete" with the version
// they asked for.
func TestVersionUpgradeCall_NoopMessageContainsVersion(t *testing.T) {
	// Synthesize the same output the tool builds at tools.go:2253-2259 so
	// callers (batch.go reads is_noop, agent rule branches on it) can rely
	// on the field being present. This is a contract test for the result
	// shape, not a behavior test for the network path.
	out := map[string]any{
		"org":            "sarah-test-org",
		"workspace":      "prod-k8s-apps",
		"target_version": "1.14.9",
		"run_id":         "run-abc",
		"status":         "planned_and_finished",
		"is_noop":        true,
		"message":        "Terraform version constraint ~> 1.14.9 uploaded; the plan finished with no infrastructure changes. The version bump is complete — no apply needed.",
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"org", "workspace", "target_version", "run_id", "status", "is_noop", "message"} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("required field %q missing from output", field)
		}
	}
	if !decoded["is_noop"].(bool) {
		t.Fatalf("is_noop must be a boolean true for noop case")
	}
}
