package tools

import (
	"strings"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.7", "1.14.9", -1},
		{"1.14.9", "1.2.7", 1},
		{"1.5.0", "1.5.0", 0},
		{"v1.5.0", "1.5.0", 0},
		{"1.5.7", "1.5.0", 1},
		{"1.0.0", "2.0.0", -1},
		// Unparseable versions fall through to lexical compare on the failed
		// component. "unknown" > "1" lexically — production never feeds raw
		// "unknown" here (callers special-case it first), so the ordering is
		// asserted only to lock down behaviour.
		{"unknown", "1.5.0", 1},
	}
	for _, tc := range tests {
		got := compareSemver(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNormalizeSeverity(t *testing.T) {
	cases := map[string]string{
		"LOW":      "low",
		"MODERATE": "medium",
		"MEDIUM":   "medium",
		"HIGH":     "high",
		"CRITICAL": "critical",
		"":         "unknown",
		"weird":    "unknown",
		" high ":   "high",
	}
	for in, want := range cases {
		if got := normalizeSeverity(in); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpgradeComplexity(t *testing.T) {
	tests := []struct {
		name      string
		resources int
		behind    int
		majorJump bool
		want      string
	}{
		{"low_few_resources_close_version", 5, 1, false, "Low"},
		{"medium_more_resources", 30, 0, false, "Medium"},
		{"medium_many_minors_behind", 5, 3, false, "Medium"},
		{"high_many_resources", 60, 0, false, "High"},
		{"high_many_minors_behind", 5, 8, false, "High"},
		{"high_major_jump", 5, 1, true, "High"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := upgradeComplexity(tc.resources, tc.behind, tc.majorJump)
			if got != tc.want {
				t.Fatalf("upgradeComplexity(%d, %d, %v) = %q, want %q",
					tc.resources, tc.behind, tc.majorJump, got, tc.want)
			}
		})
	}
}

func TestVersionsBehind(t *testing.T) {
	tests := []struct {
		current, latest string
		want            int
	}{
		{"1.2.7", "1.14.9", 12},
		{"1.14.9", "1.14.9", 0},
		{"1.5.7", "1.14.9", 9},
		{"unknown", "1.14.9", 0},
		{"1.20.0", "1.14.9", 0}, // future versions clamp to 0
	}
	for _, tc := range tests {
		if got := versionsBehind(tc.current, tc.latest); got != tc.want {
			t.Errorf("versionsBehind(%q, %q) = %d, want %d", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestParseOSVResponse_RealCVEShape(t *testing.T) {
	// Shape mirrors the real OSV.dev /v1/query response for terraform 1.2.7,
	// which returns CVE-2023-4782 (arbitrary file write, fixed in 1.5.7).
	body := []byte(`{
		"vulns": [
			{
				"id": "GHSA-h626-pv66-hhm7",
				"summary": "Terraform allows arbitrary file write during the init operation",
				"aliases": ["CVE-2023-4782", "GO-2023-2055"],
				"database_specific": {"severity": "MODERATE"},
				"affected": [
					{
						"ranges": [
							{"events": [{"introduced": "1.0.8"}, {"fixed": "1.5.7"}]}
						]
					}
				]
			}
		]
	}`)

	entries, parseErr := parseOSVResponse(body)
	if parseErr {
		t.Fatalf("parseOSVResponse reported error on valid body")
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.ID != "CVE-2023-4782" {
		t.Errorf("ID = %q, want CVE alias to be preferred", got.ID)
	}
	if got.Severity != "medium" {
		t.Errorf("Severity = %q, want medium", got.Severity)
	}
	if got.FixedIn != "1.5.7" {
		t.Errorf("FixedIn = %q, want 1.5.7", got.FixedIn)
	}
	if got.Summary == "" {
		t.Errorf("Summary should be populated")
	}
}

func TestParseOSVResponse_GarbageBody(t *testing.T) {
	if _, parseErr := parseOSVResponse([]byte("not json")); !parseErr {
		t.Errorf("expected parse error on garbage body")
	}
}

func TestParseOSVResponse_NoVulns(t *testing.T) {
	entries, parseErr := parseOSVResponse([]byte(`{"vulns": []}`))
	if parseErr {
		t.Fatalf("unexpected parse error on empty vulns")
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestResolveExactConstraint(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"4.9.0", "4.9.0", true},
		{"= 4.9.0", "4.9.0", true},
		{"==4.9.0", "4.9.0", true},
		{"v4.9.0", "4.9.0", true},
		{"~> 4.45.0", "", false},
		{">= 1.0.0", "", false},
		{"< 5.0.0", "", false},
		{"!= 4.9.0", "", false},
		{"", "", false},
		{">= 1.0, < 2.0", "", false},
		{"4.9.0-rc1", "", false},
		{"4.9", "", false},
	}
	for _, tc := range tests {
		got, ok := resolveExactConstraint(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("resolveExactConstraint(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestParseProviderConstraints(t *testing.T) {
	// Trimmed shape of mock-tfconfig-v2.sentinel: a top-level providers
	// block followed by resources and module_calls. Only the providers
	// block contributes — module_calls also use version_constraint but
	// those describe modules, not providers.
	body := []byte(`import "strings"

providers = {
	"aws": {
		"alias": "",
		"full_name":           "registry.terraform.io/hashicorp/aws",
		"name":                "aws",
		"version_constraint":  "~> 4.45.0",
	},
	"random": {
		"alias": "",
		"full_name":           "registry.terraform.io/hashicorp/random",
		"name":                "random",
		"version_constraint":  "3.5.0",
	},
}

resources = {
	"aws_security_group.app": {
		"address": "aws_security_group.app",
	},
}

module_calls = {
	"vpc": {
		"source":             "terraform-aws-modules/vpc/aws",
		"version_constraint": "3.14.0",
	},
}
`)
	got := parseProviderConstraints(body)
	if got == nil {
		t.Fatal("parseProviderConstraints returned nil; want map with two providers")
	}
	if got["hashicorp/aws"] != "~> 4.45.0" {
		t.Errorf("hashicorp/aws = %q, want ~> 4.45.0", got["hashicorp/aws"])
	}
	if got["hashicorp/random"] != "3.5.0" {
		t.Errorf("hashicorp/random = %q, want 3.5.0", got["hashicorp/random"])
	}
	if _, ok := got["terraform-aws-modules/vpc/aws"]; ok {
		t.Errorf("module-level version_constraint leaked into provider map")
	}
}

func TestParseProviderConstraints_NoProvidersBlock(t *testing.T) {
	if got := parseProviderConstraints([]byte("resources = {}")); got != nil {
		t.Errorf("expected nil for body with no providers block, got %v", got)
	}
}

func TestComputeCVEDiff_PinnedKnown(t *testing.T) {
	cves := []advisoryEntry{
		{ID: "CVE-A", Severity: "high", FixedIn: "5.0.0"},   // pinned (4.9.0) < 5.0.0; fixed_in <= latest (5.91.0) → both
		{ID: "CVE-B", Severity: "medium", FixedIn: "4.5.0"}, // pinned (4.9.0) >= 4.5.0 → neither
		{ID: "CVE-C", Severity: "high", FixedIn: ""},        // no fix → currently_affected only
		{ID: "CVE-D", Severity: "low", FixedIn: "5.91.0"},   // boundary: fixed_in == latest → both
	}
	currently, fixes := computeCVEDiff("4.9.0", "5.91.0", cves)

	currentIDs := map[string]bool{}
	for _, c := range currently {
		currentIDs[c.ID] = true
	}
	fixIDs := map[string]bool{}
	for _, c := range fixes {
		fixIDs[c.ID] = true
	}

	for _, want := range []string{"CVE-A", "CVE-C", "CVE-D"} {
		if !currentIDs[want] {
			t.Errorf("currently_affected missing %s", want)
		}
	}
	if currentIDs["CVE-B"] {
		t.Errorf("currently_affected should not include CVE-B (fix predates pinned)")
	}
	for _, want := range []string{"CVE-A", "CVE-D"} {
		if !fixIDs[want] {
			t.Errorf("upgrading_fixes missing %s", want)
		}
	}
	if fixIDs["CVE-B"] || fixIDs["CVE-C"] {
		t.Errorf("upgrading_fixes should not include CVE-B (already patched) or CVE-C (no fix)")
	}
}

func TestComputeCVEDiff_NonNilSlices(t *testing.T) {
	currently, fixes := computeCVEDiff("4.9.0", "5.91.0", nil)
	if currently == nil || fixes == nil {
		t.Errorf("computeCVEDiff returned nil slices; want empty slices for stable JSON output")
	}
}

func TestBuildProviderUpgradeNote(t *testing.T) {
	cases := []struct {
		name                                       string
		pinned, latest                             string
		all, currently, fixes                      int
		wantContains                               string
	}{
		{"unknown_with_cves", "unknown", "5.91.0", 4, 0, 4, "Pinned version is unknown — upgrading to 5.91.0 addresses all 4 known CVEs"},
		{"unknown_no_cves", "unknown", "5.91.0", 0, 0, 0, "No known CVEs"},
		{"latest_unavailable", "unknown", "unavailable", 0, 0, 0, "Could not determine latest"},
		{"already_latest_with_cves", "5.91.0", "5.91.0", 2, 1, 0, "1 known CVE still affect"},
		{"upgrade_resolves", "4.9.0", "5.91.0", 3, 2, 2, "upgrading to 5.91.0 would resolve 2 known CVEs"},
		{"behind_no_cves", "4.9.0", "5.91.0", 0, 0, 0, "None of the 0 known CVEs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProviderUpgradeNote(tc.pinned, tc.latest, tc.all, tc.currently, tc.fixes)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("note = %q, want to contain %q", got, tc.wantContains)
			}
		})
	}
}

func TestParseOSVResponse_DedupsByID(t *testing.T) {
	// OSV often returns both the GHSA and GO-* records for the same CVE.
	// The aliases field aligns them on the same canonical CVE id; we should
	// emit only one entry.
	body := []byte(`{
		"vulns": [
			{"id": "GHSA-aaaa", "summary": "first", "aliases": ["CVE-2024-9999"],
			 "database_specific": {"severity": "HIGH"},
			 "affected": [{"ranges": [{"events": [{"fixed": "1.0.0"}]}]}]},
			{"id": "GO-2024-0001", "summary": "duplicate", "aliases": ["CVE-2024-9999"],
			 "database_specific": {"severity": "HIGH"},
			 "affected": [{"ranges": [{"events": [{"fixed": "1.0.0"}]}]}]}
		]
	}`)
	entries, parseErr := parseOSVResponse(body)
	if parseErr {
		t.Fatalf("unexpected parse error")
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries after dedup, want 1", len(entries))
	}
}
