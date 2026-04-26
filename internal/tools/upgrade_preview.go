package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// upgradePreviewCall implements _hcp_tf_upgrade_preview. It rewrites the
// requested provider's version constraint in a copy of the local HCL,
// uploads the result as a speculative configuration version, polls for the
// auto-queued plan-only run, feeds it through planAnalyzeCall for risk +
// blast radius, cross-references upgrading_fixes from providerAuditCall for
// the CVE delta, and pulls breaking changes out of GitHub release notes.
// The speculative run is discarded after analysis (best-effort).
//
// Speculative configversions auto-queue a plan-only run when -auto-queue-runs
// is true (the hcptf default). hcptf run create does not accept a
// -configuration-version flag, so the auto-queue mechanism is the only path
// to a speculative run wired to a specific configversion.
func upgradePreviewCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_upgrade_preview", Args: args}

	if err := require(args, "org", "workspace", "provider", "target_version"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]
	provider := strings.TrimSpace(args["provider"])
	targetVersion := strings.TrimPrefix(strings.TrimSpace(args["target_version"]), "v")
	configPath := args["config_path"]
	if configPath == "" {
		cwd, gerr := os.Getwd()
		if gerr != nil {
			result.Err = &ToolError{ErrorCode: "execution_error", Message: "could not resolve current directory: " + gerr.Error()}
			result.Duration = time.Since(start)
			return result
		}
		configPath = cwd
	}
	if _, err := os.Stat(configPath); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: fmt.Sprintf("config_path does not exist: %s", configPath)}
		result.Duration = time.Since(start)
		return result
	}
	tfFiles, _ := filepath.Glob(filepath.Join(configPath, "*.tf"))
	if len(tfFiles) == 0 {
		result.Err = &ToolError{
			ErrorCode: "unsupported_operation",
			Message:   fmt.Sprintf("no .tf files found in %s — tfpilot must be launched from a directory containing the workspace's HCL to preview an upgrade", configPath),
		}
		result.Duration = time.Since(start)
		return result
	}

	// Step 1: provider audit for current version + CVE delta context.
	auditRes := providerAuditCall(ctx, map[string]string{"org": org, "workspace": workspace}, timeoutSec)
	if auditRes.Err != nil {
		result.Err = auditRes.Err
		result.Duration = time.Since(start)
		return result
	}
	pinnedVersion, pinnedSource, upgradingFixes, providerKnown := lookupProviderInAudit(auditRes.Output, provider)
	if !providerKnown {
		result.Err = &ToolError{
			ErrorCode: "invalid_tool",
			Message:   fmt.Sprintf("provider %q not found in workspace %q — call _hcp_tf_provider_audit to see available providers", provider, workspace),
		}
		result.Duration = time.Since(start)
		return result
	}

	// Step 2: stage HCL into a tempdir and rewrite the version constraint.
	stageDir, terr := os.MkdirTemp("", "tfpilot-upgrade-*")
	if terr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "tempdir: " + terr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	defer os.RemoveAll(stageDir)
	if cerr := copyTFFiles(configPath, stageDir); cerr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "stage HCL: " + cerr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	mutated, merr := mutateProviderVersion(stageDir, provider, targetVersion)
	if merr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "rewrite version: " + merr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	if mutated == 0 {
		result.Err = &ToolError{
			ErrorCode: "unsupported_operation",
			Message:   fmt.Sprintf("could not find provider %q in any required_providers or provider block under %s — add it to your terraform.required_providers block and try again", provider, configPath),
		}
		result.Duration = time.Since(start)
		return result
	}

	// Step 3: create speculative configversion + upload.
	cvRaw, cvErr := fetchHCPTFJSON(ctx, timeoutSec, "configversion", "create",
		"-org="+org, "-workspace="+workspace, "-speculative", "-output=json")
	if cvErr != nil {
		result.Err = cvErr
		result.Duration = time.Since(start)
		return result
	}
	cvID, uploadURL := decodeConfigVersionCreate(cvRaw)
	if cvID == "" {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "configversion create did not return an ID"}
		result.Duration = time.Since(start)
		return result
	}
	if uploadURL == "" {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "configversion create did not return an UploadURL"}
		result.Duration = time.Since(start)
		return result
	}
	tarball, tgErr := tarGzDir(stageDir)
	if tgErr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "tar.gz: " + tgErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	if uerr := putArchivist(ctx, uploadURL, tarball, timeoutSec); uerr != nil {
		result.Err = uerr
		result.Duration = time.Since(start)
		return result
	}

	// Step 4: poll for the auto-queued speculative run on this configversion,
	// then wait for it to reach a terminal plan state.
	runID, prErr := waitForSpeculativeRun(ctx, org, workspace, cvID, timeoutSec)
	if prErr != nil {
		result.Err = prErr
		result.Duration = time.Since(start)
		return result
	}

	// Step 5: analyze the speculative plan.
	analyzeRes := planAnalyzeCall(ctx, map[string]string{"org": org, "workspace": workspace, "run_id": runID}, timeoutSec)
	// Discard regardless of analyze outcome — speculative runs cannot apply but
	// discarding is harmless and matches the explicit cleanup contract.
	defer func() {
		_, _ = fetchHCPTFJSON(context.Background(), timeoutSec, "run", "discard",
			"-id="+runID, "-comment=tfpilot: upgrade preview cleanup")
	}()
	if analyzeRes.Err != nil {
		result.Err = analyzeRes.Err
		result.Duration = time.Since(start)
		return result
	}
	var analyzed map[string]any
	if jerr := json.Unmarshal(analyzeRes.Output, &analyzed); jerr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: "decode plan_analyze: " + jerr.Error()}
		result.Duration = time.Since(start)
		return result
	}

	// Step 6: GitHub release notes for breaking changes.
	breakingChanges, breakingSource := fetchProviderReleaseNotes(ctx, provider, pinnedVersion, targetVersion)

	// Step 7: filter CVEs that the upgrade actually closes (fixed_in <= target).
	cvesFixed := filterCVEsFixedBy(upgradingFixes, targetVersion)

	// Step 8: synthesize go/review/no_go recommendation.
	riskLevel, _ := analyzed["risk_level"].(string)
	blastRadius, _ := analyzed["blast_radius"].(map[string]any)
	recommendation, reason := synthesizeUpgradeRecommendation(
		riskLevel,
		blastRadius,
		breakingChanges,
		cvesFixed,
		pinnedVersion,
		pinnedSource,
		targetVersion,
	)

	payload := map[string]any{
		"workspace":               workspace,
		"org":                     org,
		"provider":                "hashicorp/" + provider,
		"from_version":            pinnedVersion,
		"from_version_source":     pinnedSource,
		"target_version":          targetVersion,
		"speculative_run_id":      runID,
		"risk_level":              riskLevel,
		"risk_factors":            analyzed["risk_factors"],
		"blast_radius":            blastRadius,
		"cves_fixed":              cvesFixed,
		"breaking_changes":        breakingChanges,
		"breaking_changes_source": breakingSource,
		"recommendation":          recommendation,
		"recommendation_reason":   reason,
	}
	out, mErr := json.Marshal(payload)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// lookupProviderInAudit pulls pinned_version, pinned_version_source, and
// upgrading_fixes for a single provider out of the providerAuditCall payload.
// The third return is true when the provider name appears in the audit's
// providers array (regardless of whether pinned_version is "unknown").
func lookupProviderInAudit(raw json.RawMessage, providerShort string) (string, string, []advisoryEntry, bool) {
	var env struct {
		PinnedSource string `json:"pinned_version_source"`
		Providers    []struct {
			Name           string          `json:"name"`
			PinnedVersion  string          `json:"pinned_version"`
			UpgradingFixes []advisoryEntry `json:"upgrading_fixes"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "unknown", "unknown", nil, false
	}
	for _, p := range env.Providers {
		if strings.EqualFold(p.Name, providerShort) {
			return p.PinnedVersion, env.PinnedSource, p.UpgradingFixes, true
		}
	}
	return "unknown", env.PinnedSource, nil, false
}

// copyTFFiles copies every .tf file (and .tfvars) from src to dst at the top
// level only — nested module directories are intentionally skipped because the
// version constraint we mutate lives in the root-level required_providers
// block. Files in modules will get re-resolved by HCP Terraform's init.
func copyTFFiles(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tf") && !strings.HasSuffix(name, ".tfvars") && name != ".terraform.lock.hcl" {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			return rerr
		}
		if werr := os.WriteFile(filepath.Join(dst, name), body, 0o644); werr != nil {
			return werr
		}
		copied++
	}
	if copied == 0 {
		return fmt.Errorf("no .tf files in %s", src)
	}
	return nil
}

// mutateProviderVersion rewrites the version string for the named provider in
// every .tf file under dir. Handles two patterns:
//
//  1. terraform { required_providers { <name> = { source = "...", version = "..." } } }
//  2. provider "<name>" { version = "..." } (legacy form)
//
// Returns the number of files modified. Zero return means the provider was
// not found in any HCL file under dir.
func mutateProviderVersion(dir, provider, targetVersion string) (int, error) {
	target := `"= ` + targetVersion + `"`
	// Match `<provider> = { ... version = "..." ... }` inside required_providers.
	reReq := regexp.MustCompile(`(?ms)(\b` + regexp.QuoteMeta(provider) + `\s*=\s*\{[^}]*?version\s*=\s*)"[^"]*"`)
	// Match `provider "<name>" { ... version = "..." ... }` (legacy).
	reLegacy := regexp.MustCompile(`(?ms)(provider\s+"` + regexp.QuoteMeta(provider) + `"\s*\{[^}]*?version\s*=\s*)"[^"]*"`)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	modified := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return modified, rerr
		}
		orig := body
		body = reReq.ReplaceAll(body, []byte(`${1}`+target))
		body = reLegacy.ReplaceAll(body, []byte(`${1}`+target))
		if !bytes.Equal(orig, body) {
			if werr := os.WriteFile(path, body, 0o644); werr != nil {
				return modified, werr
			}
			modified++
		}
	}
	return modified, nil
}

// decodeConfigVersionCreate pulls the configversion ID and one-shot upload URL
// out of the JSON returned by `hcptf configversion create`. Tolerates a few
// field-name variants because hcptf has shifted casing across versions.
func decodeConfigVersionCreate(raw []byte) (string, string) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", ""
	}
	id := ""
	for _, k := range []string{"ID", "id", "ConfigurationVersionID", "configuration_version_id"} {
		if v, ok := parsed[k].(string); ok && v != "" {
			id = v
			break
		}
	}
	url := ""
	for _, k := range []string{"UploadURL", "upload_url"} {
		if v, ok := parsed[k].(string); ok && v != "" {
			url = v
			break
		}
	}
	return id, url
}

// waitForSpeculativeRun polls hcptf run list for a run whose ConfigurationVersionID
// matches cvID, then waits for that run to reach a terminal plan state. Returns
// the run ID once planned_and_finished. Times out after 5 minutes.
//
// HCP Terraform auto-queues a speculative run when a speculative configversion
// finishes uploading (with auto-queue-runs=true, the hcptf default), so the new
// run typically appears within a few seconds. We snapshot the recent run IDs
// before polling so we only need to inspect runs newer than our upload.
func waitForSpeculativeRun(ctx context.Context, org, workspace, cvID string, timeoutSec int) (string, *ToolError) {
	deadline := time.Now().Add(5 * time.Minute)
	var runID string

	// First phase: discover the auto-queued run via configversion match.
	for runID == "" && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", &ToolError{ErrorCode: "execution_error", Message: "context canceled while waiting for speculative run"}
		default:
		}
		listRaw, lerr := fetchHCPTFJSON(ctx, timeoutSec, "run", "list", "-org="+org, "-workspace="+workspace, "-output=json")
		if lerr != nil {
			return "", lerr
		}
		var runs []map[string]any
		if err := json.Unmarshal(listRaw, &runs); err != nil {
			return "", &ToolError{ErrorCode: "marshal_error", Message: "decode run list: " + err.Error()}
		}
		// Inspect the most recent ~5 runs to find the one tied to our cvID.
		limit := 5
		if len(runs) < limit {
			limit = len(runs)
		}
		for i := 0; i < limit; i++ {
			id := firstStringField(runs[i], "ID", "id")
			if id == "" {
				continue
			}
			showRaw, serr := fetchHCPTFJSON(ctx, timeoutSec, "run", "show", "-id="+id, "-output=json")
			if serr != nil {
				continue
			}
			var show map[string]any
			if err := json.Unmarshal(showRaw, &show); err != nil {
				continue
			}
			if firstStringField(show, "ConfigurationVersionID", "configuration_version_id") == cvID {
				runID = id
				break
			}
		}
		if runID == "" {
			time.Sleep(3 * time.Second)
		}
	}
	if runID == "" {
		return "", &ToolError{
			ErrorCode: "execution_error",
			Message:   "timed out waiting for speculative run to be auto-queued (workspace may have auto-queue-runs disabled or be VCS-driven)",
		}
	}

	// Second phase: wait for the run to reach a terminal plan state.
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return runID, &ToolError{ErrorCode: "execution_error", Message: "context canceled while waiting for plan to finish"}
		default:
		}
		showRaw, serr := fetchHCPTFJSON(ctx, timeoutSec, "run", "show", "-id="+runID, "-output=json")
		if serr != nil {
			return runID, serr
		}
		var show map[string]any
		_ = json.Unmarshal(showRaw, &show)
		status := firstStringField(show, "Status", "status")
		switch status {
		case "planned", "planned_and_finished", "cost_estimated", "policy_checked":
			return runID, nil
		case "errored", "canceled", "discarded":
			return runID, &ToolError{
				ErrorCode: "execution_error",
				Message:   fmt.Sprintf("speculative run %s ended with status %q before producing a plan", runID, status),
			}
		}
		time.Sleep(4 * time.Second)
	}
	return runID, &ToolError{
		ErrorCode: "execution_error",
		Message:   fmt.Sprintf("speculative run %s did not reach a planned state within 5 minutes", runID),
	}
}

// breakingChangeKeywords are the substrings we treat as signals that a release
// note line announces a breaking change. Case-insensitive match.
var breakingChangeKeywords = []string{
	"breaking change",
	"breaking:",
	"removed:",
	"deprecated:",
	"no longer",
	"must now",
	"is now required",
}

// breakingSectionRE finds a `BREAKING CHANGES:` section inside an AWS-style
// release body and captures the bullet-list block until the next ALL-CAPS
// section header (e.g. NOTES:, FEATURES:, ENHANCEMENTS:, BUG FIXES:).
var breakingSectionRE = regexp.MustCompile(`(?is)BREAKING\s+CHANGES?\s*:?\s*\n(.*?)(?:\n[A-Z][A-Z\s]+:|\z)`)

// fetchProviderReleaseNotes hits the GitHub Releases API for the named
// provider and returns plain-English breaking-change summaries between
// fromVersion (exclusive) and toVersion (inclusive). Honors GITHUB_TOKEN.
// Returns ([summaries], "github_releases") on success, or ([], "unavailable")
// when the API is unreachable, rate-limited, or has no relevant releases.
func fetchProviderReleaseNotes(ctx context.Context, providerShort, fromVersion, toVersion string) ([]string, string) {
	url := fmt.Sprintf("https://api.github.com/repos/hashicorp/terraform-provider-%s/releases?per_page=100", providerShort)
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return []string{}, "unavailable"
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tfpilot/1.6")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []string{}, "unavailable"
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return []string{"GitHub API rate limit reached — set GITHUB_TOKEN for higher limits"}, "rate_limited"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return []string{}, "unavailable"
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return []string{}, "unavailable"
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if jerr := json.Unmarshal(body, &releases); jerr != nil {
		return []string{}, "unavailable"
	}

	type relevant struct {
		version string
		body    string
	}
	var picked []relevant
	for _, r := range releases {
		v := strings.TrimPrefix(r.TagName, "v")
		// Skip pre-release tags (alpha/beta/rc).
		if strings.ContainsAny(v, "abrc-") && !strings.HasPrefix(v, "0.") {
			// Heuristic: if the version contains common pre-release markers
			// after a digit, skip it. compareSemver lexically falls back when
			// it can't parse, which would distort the range.
			if strings.Contains(v, "-alpha") || strings.Contains(v, "-beta") || strings.Contains(v, "-rc") {
				continue
			}
		}
		if fromVersion != "unknown" && fromVersion != "" {
			if compareSemver(v, fromVersion) <= 0 {
				continue
			}
		}
		if compareSemver(v, toVersion) > 0 {
			continue
		}
		picked = append(picked, relevant{version: v, body: r.Body})
	}
	sort.Slice(picked, func(i, j int) bool {
		return compareSemver(picked[i].version, picked[j].version) < 0
	})

	summaries := []string{}
	seen := map[string]bool{}
	for _, p := range picked {
		for _, line := range extractBreakingLines(p.body) {
			key := strings.ToLower(line)
			if seen[key] {
				continue
			}
			seen[key] = true
			summaries = append(summaries, fmt.Sprintf("%s: %s", p.version, line))
			if len(summaries) >= 20 {
				break
			}
		}
		if len(summaries) >= 20 {
			break
		}
	}
	if len(summaries) == 0 {
		return []string{}, "github_releases"
	}
	return summaries, "github_releases"
}

// extractBreakingLines scans a release-note body for explicit BREAKING CHANGES
// sections first, then falls back to scanning every line for the keyword set.
// Each returned string is a single human-readable line, trimmed of markdown
// list markers and trailing whitespace.
func extractBreakingLines(body string) []string {
	if body == "" {
		return nil
	}
	out := []string{}

	if m := breakingSectionRE.FindStringSubmatch(body); len(m) >= 2 {
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "*")
			line = strings.TrimPrefix(line, "-")
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Strip GitHub issue-link suffix `([#12345](...))` which is noisy.
			if i := strings.Index(line, "(["); i > 0 {
				line = strings.TrimSpace(line[:i])
			}
			out = append(out, line)
		}
		if len(out) > 0 {
			return out
		}
	}

	// Fallback: keyword scan across the whole body.
	for _, line := range strings.Split(body, "\n") {
		lower := strings.ToLower(line)
		matched := false
		for _, kw := range breakingChangeKeywords {
			if strings.Contains(lower, kw) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.Index(line, "(["); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		out = append(out, line)
	}
	return out
}

// filterCVEsFixedBy keeps only the advisories whose FixedIn version is at or
// before targetVersion — i.e. CVEs the upgrade actually closes. Advisories
// without a FixedIn are kept (no version data → conservatively assume the
// upgrade addresses them, since they came from the upgrading_fixes set).
func filterCVEsFixedBy(advisories []advisoryEntry, targetVersion string) []advisoryEntry {
	out := []advisoryEntry{}
	for _, a := range advisories {
		if a.FixedIn == "" || compareSemver(a.FixedIn, targetVersion) <= 0 {
			out = append(out, a)
		}
	}
	return out
}

// synthesizeUpgradeRecommendation collapses the four signal sources (plan
// risk, breaking changes, CVE fixes, version-source confidence) into a single
// go|review|no_go recommendation with a plain-English reason.
//
// no_go: risk_level Critical, OR breaking_changes mention a resource type
// that appears in the plan's blast radius highest_risk_resources.
// review: risk_level High, OR any breaking changes present, OR
// from_version_source is "unknown" (we cannot trust the version delta).
// go: Low/Medium risk AND no breaking changes AND at least one CVE fixed.
// Otherwise: review.
func synthesizeUpgradeRecommendation(
	riskLevel string,
	blastRadius map[string]any,
	breakingChanges []string,
	cvesFixed []advisoryEntry,
	pinnedVersion, pinnedSource, targetVersion string,
) (string, string) {
	risk := strings.ToLower(strings.TrimSpace(riskLevel))
	hasBreaking := len(breakingChanges) > 0 && !(len(breakingChanges) == 1 && strings.HasPrefix(breakingChanges[0], "GitHub API rate limit"))
	cveCount := len(cvesFixed)

	// Critical risk is an automatic no_go.
	if risk == "critical" {
		return "no_go", fmt.Sprintf("Speculative plan came back as Critical risk. Do not proceed with this upgrade as written; resolve the underlying risk factors first.")
	}

	// Breaking changes that touch resource types in the plan's blast radius
	// are also an automatic no_go.
	if hasBreaking && breakingChangeIntersectsPlan(breakingChanges, blastRadius) {
		return "no_go", fmt.Sprintf("Breaking changes in the %s release notes touch resource types that this workspace manages. Manual remediation required before the upgrade is safe.", targetVersion)
	}

	if risk == "high" {
		return "review", fmt.Sprintf("Speculative plan came back as High risk (%d resources affected). Walk through the plan output before applying.", totalAffected(blastRadius))
	}

	if hasBreaking {
		return "review", fmt.Sprintf("%d breaking change(s) found in upstream release notes between %s and %s. Verify each against your config before applying.", len(breakingChanges), pinnedVersion, targetVersion)
	}

	if pinnedSource == "unknown" || pinnedVersion == "unknown" || pinnedVersion == "" {
		return "review", "Could not determine the workspace's currently pinned provider version (plan export unavailable). Confirm the pinned version manually before treating this as a safe upgrade."
	}

	if cveCount > 0 {
		return "go", fmt.Sprintf("Speculative plan is %s risk, no breaking changes detected, and the upgrade closes %d CVE(s).", riskLevel, cveCount)
	}

	return "review", fmt.Sprintf("Speculative plan is %s risk and no breaking changes detected, but the upgrade does not close any known CVEs — confirm the upgrade is worth the change.", riskLevel)
}

// breakingChangeIntersectsPlan returns true when any breaking-change line
// mentions a resource type that appears in highest_risk_resources of the plan.
// Heuristic: extract aws_*-style identifiers from the highest-risk list and
// substring-match them against the lowercased breaking-change text.
func breakingChangeIntersectsPlan(breakingChanges []string, blastRadius map[string]any) bool {
	if blastRadius == nil {
		return false
	}
	highest, ok := blastRadius["highest_risk_resources"].([]any)
	if !ok {
		return false
	}
	types := map[string]bool{}
	for _, item := range highest {
		switch v := item.(type) {
		case string:
			if rt := typeFromAddress(v); rt != "" {
				types[strings.ToLower(rt)] = true
			}
		case map[string]any:
			if rt := firstStringField(v, "resource_type", "type", "ResourceType"); rt != "" {
				types[strings.ToLower(rt)] = true
			} else if addr := firstStringField(v, "address", "Address"); addr != "" {
				if rt := typeFromAddress(addr); rt != "" {
					types[strings.ToLower(rt)] = true
				}
			}
		}
	}
	if len(types) == 0 {
		return false
	}
	for _, line := range breakingChanges {
		lower := strings.ToLower(line)
		for rt := range types {
			if strings.Contains(lower, rt) {
				return true
			}
		}
	}
	return false
}

func totalAffected(blastRadius map[string]any) int {
	if blastRadius == nil {
		return 0
	}
	if v, ok := blastRadius["total_resources_affected"].(float64); ok {
		return int(v)
	}
	if v, ok := blastRadius["total_resources_affected"].(int); ok {
		return v
	}
	return 0
}

