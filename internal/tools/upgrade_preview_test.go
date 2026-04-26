package tools

import (
	"os"
	"testing"
)

func TestSynthesizeUpgradeRecommendation(t *testing.T) {
	cve := []advisoryEntry{{ID: "CVE-2026-0001", Severity: "high", FixedIn: "5.50.0"}}

	cases := []struct {
		name          string
		risk          string
		blast         map[string]any
		breaking      []string
		cves          []advisoryEntry
		pinned        string
		pinnedSource  string
		target        string
		wantDecision  string
		wantReasonHas string
	}{
		{
			name:          "critical risk -> no_go",
			risk:          "Critical",
			blast:         map[string]any{"total_resources_affected": float64(50)},
			breaking:      nil,
			cves:          cve,
			pinned:        "5.0.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "no_go",
			wantReasonHas: "Critical",
		},
		{
			name:  "breaking change touches resource type in plan -> no_go",
			risk:  "Medium",
			blast: map[string]any{"highest_risk_resources": []any{"aws_s3_bucket.logs", "aws_iam_role.app"}},
			breaking: []string{
				"5.0.0: aws_s3_bucket split into separate resources",
			},
			cves:          cve,
			pinned:        "4.45.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "no_go",
			wantReasonHas: "Manual remediation",
		},
		{
			name:          "high risk with no breaking change intersection -> review",
			risk:          "High",
			blast:         map[string]any{"total_resources_affected": float64(36), "highest_risk_resources": []any{"aws_kinesis_stream.events"}},
			breaking:      []string{"5.0.0: aws_s3_bucket split"},
			cves:          cve,
			pinned:        "4.45.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "review",
			wantReasonHas: "High risk",
		},
		{
			name:          "low risk + breaking change present -> review",
			risk:          "Low",
			blast:         map[string]any{"total_resources_affected": float64(4), "highest_risk_resources": []any{"aws_lambda_function.handler"}},
			breaking:      []string{"5.0.0: aws_s3_bucket split"},
			cves:          cve,
			pinned:        "4.45.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "review",
			wantReasonHas: "breaking change",
		},
		{
			name:          "unknown pinned version -> review even when otherwise green",
			risk:          "Low",
			blast:         map[string]any{"total_resources_affected": float64(2)},
			breaking:      nil,
			cves:          cve,
			pinned:        "unknown",
			pinnedSource:  "unknown",
			target:        "5.91.0",
			wantDecision:  "review",
			wantReasonHas: "currently pinned",
		},
		{
			name:          "low risk, no breaking, CVEs fixed -> go",
			risk:          "Low",
			blast:         map[string]any{"total_resources_affected": float64(2)},
			breaking:      nil,
			cves:          cve,
			pinned:        "4.45.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "go",
			wantReasonHas: "closes 1 CVE",
		},
		{
			name:          "rate-limit-only line is treated as no breaking changes",
			risk:          "Low",
			blast:         map[string]any{"total_resources_affected": float64(2)},
			breaking:      []string{"GitHub API rate limit reached — set GITHUB_TOKEN for higher limits"},
			cves:          cve,
			pinned:        "4.45.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "go",
			wantReasonHas: "closes 1 CVE",
		},
		{
			name:          "low risk, no breaking, no CVEs -> review (upgrade buys nothing)",
			risk:          "Medium",
			blast:         map[string]any{"total_resources_affected": float64(8)},
			breaking:      nil,
			cves:          nil,
			pinned:        "5.85.0",
			pinnedSource:  "planexport",
			target:        "5.91.0",
			wantDecision:  "review",
			wantReasonHas: "does not close any known CVEs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := synthesizeUpgradeRecommendation(tc.risk, tc.blast, tc.breaking, tc.cves, tc.pinned, tc.pinnedSource, tc.target)
			if got != tc.wantDecision {
				t.Fatalf("decision: got %q, want %q (reason: %s)", got, tc.wantDecision, reason)
			}
			if !contains(reason, tc.wantReasonHas) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantReasonHas)
			}
		})
	}
}

func TestFilterCVEsFixedBy(t *testing.T) {
	cves := []advisoryEntry{
		{ID: "CVE-A", FixedIn: "4.50.0"},
		{ID: "CVE-B", FixedIn: "5.91.0"},
		{ID: "CVE-C", FixedIn: "6.0.0"}, // not closed by 5.91.0 target
		{ID: "CVE-D", FixedIn: ""},      // no FixedIn → kept
	}
	out := filterCVEsFixedBy(cves, "5.91.0")
	gotIDs := map[string]bool{}
	for _, a := range out {
		gotIDs[a.ID] = true
	}
	if !gotIDs["CVE-A"] || !gotIDs["CVE-B"] || !gotIDs["CVE-D"] {
		t.Fatalf("expected CVE-A, CVE-B, CVE-D, got %+v", gotIDs)
	}
	if gotIDs["CVE-C"] {
		t.Fatalf("CVE-C (fixed in 6.0.0) should be excluded for target 5.91.0, got %+v", gotIDs)
	}
}

func TestMutateProviderVersion(t *testing.T) {
	dir := t.TempDir()
	must := func(path, body string) {
		if err := writeFile(dir, path, body); err != nil {
			t.Fatal(err)
		}
	}
	must("main.tf", `terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 4.45.0"
    }
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0.0"
    }
  }
}

resource "aws_s3_bucket" "test" {
  bucket = "tfpilot-test"
}
`)
	must("legacy.tf", `provider "aws" {
  version = "4.45.0"
  region  = "us-east-1"
}
`)
	mod, err := mutateProviderVersion(dir, "aws", "5.91.0")
	if err != nil {
		t.Fatal(err)
	}
	if mod < 1 {
		t.Fatalf("expected at least 1 file modified, got %d", mod)
	}
	body, _ := readFile(dir, "main.tf")
	if !contains(body, `version = "= 5.91.0"`) {
		t.Fatalf("main.tf did not get aws version rewritten:\n%s", body)
	}
	if !contains(body, `>= 5.0.0`) {
		t.Fatalf("main.tf google version should be unchanged:\n%s", body)
	}
	body2, _ := readFile(dir, "legacy.tf")
	if !contains(body2, `version = "= 5.91.0"`) {
		t.Fatalf("legacy.tf did not get aws version rewritten:\n%s", body2)
	}
}

func TestExtractBreakingLinesAWSStyle(t *testing.T) {
	body := `## 5.0.0 (April 1, 2024)

BREAKING CHANGES:

* resource/aws_s3_bucket: Split into separate resources ([#12345](https://github.com/...))
* provider: Region is now required

NOTES:

* Some unrelated note
`
	out := extractBreakingLines(body)
	if len(out) < 2 {
		t.Fatalf("expected 2 breaking lines, got %d: %+v", len(out), out)
	}
	if !contains(out[0], "Split into separate resources") {
		t.Fatalf("first line wrong: %q", out[0])
	}
	if contains(out[0], "[#12345]") {
		t.Fatalf("issue link suffix should be stripped: %q", out[0])
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func writeFile(dir, name, body string) error {
	return os.WriteFile(dir+"/"+name, []byte(body), 0o644)
}

func readFile(dir, name string) (string, error) {
	b, err := os.ReadFile(dir + "/" + name)
	return string(b), err
}
