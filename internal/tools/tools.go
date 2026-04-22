package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MutatingTools is the set of tool names that cause a state change in HCP
// Terraform. The REPL approval gate and the readonly-mode filter key off this
// set.
var MutatingTools = map[string]bool{
	"_hcp_tf_run_create":  true,
	"_hcp_tf_run_apply":   true,
	"_hcp_tf_run_discard": true,
}

// IsMutating reports whether a tool name triggers state changes.
func IsMutating(name string) bool { return MutatingTools[name] }

type ToolError struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *ToolError) Error() string { return e.Message }

type CallResult struct {
	ToolName string
	Args     map[string]string
	Output   json.RawMessage
	Err      *ToolError
	Duration time.Duration
}

func Call(ctx context.Context, name string, args map[string]string, timeoutSec int) *CallResult {
	result := callDispatch(ctx, name, args, timeoutSec)
	writeAuditLog(name, args, result)
	return result
}

// LogCancellation records a synthesized tool-call result in the audit log —
// used when the approval gate rejects a mutation and the tool never actually
// executes, so there is still a durable record of the attempt.
func LogCancellation(name string, args map[string]string, result *CallResult) {
	writeAuditLog(name, args, result)
}

// configValidateCall runs `terraform validate -json` in the given directory.
// When `.terraform` is absent, a best-effort `terraform init -backend=false
// -input=false` is run first so providers referenced by the config can be
// resolved; init failures are surfaced but do not short-circuit validation.
func configValidateCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_config_validate", Args: args}

	if err := require(args, "config_path"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	dir := args["config_path"]
	if _, err := os.Stat(dir); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: fmt.Sprintf("config_path does not exist: %s", dir), Retryable: false}
		result.Duration = time.Since(start)
		return result
	}

	if _, err := exec.LookPath("terraform"); err != nil {
		result.Err = &ToolError{ErrorCode: "terraform_not_found", Message: "terraform CLI not found on PATH", Retryable: false}
		result.Duration = time.Since(start)
		return result
	}

	if _, err := os.Stat(filepath.Join(dir, ".terraform")); err != nil {
		ictx, icancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer icancel()
		initCmd := exec.CommandContext(ictx, "terraform", "init", "-backend=false", "-input=false", "-no-color")
		initCmd.Dir = dir
		_ = initCmd.Run()
	}

	vctx, vcancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer vcancel()
	cmd := exec.CommandContext(vctx, "terraform", "validate", "-json")
	cmd.Dir = dir
	out, execErr := cmd.Output()
	result.Duration = time.Since(start)

	// `terraform validate -json` always prints a JSON document and exits 0 when
	// valid, 1 when invalid; both cases are reported through the document.
	if len(out) > 0 {
		var tfResp struct {
			Valid       bool `json:"valid"`
			Diagnostics []struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Detail   string `json:"detail"`
			} `json:"diagnostics"`
		}
		if jerr := json.Unmarshal(out, &tfResp); jerr == nil {
			errs := []map[string]string{}
			for _, d := range tfResp.Diagnostics {
				if d.Severity == "error" {
					errs = append(errs, map[string]string{"summary": d.Summary, "detail": d.Detail})
				}
			}
			summary := map[string]any{"valid": tfResp.Valid, "errors": errs}
			enc, _ := json.Marshal(summary)
			result.Output = json.RawMessage(enc)
			return result
		}
	}

	// Unparseable output: treat any non-zero exit as a generic execution error.
	if execErr != nil {
		result.Err = normalizeExecError(execErr, vctx, out, timeoutSec)
		return result
	}
	result.Output = json.RawMessage(out)
	return result
}

// prCreateCall opens a pull request for generated configuration. It creates
// and pushes a branch from the current HEAD, then calls `gh pr create`. The
// caller is responsible for having staged file contents on disk before
// invocation.
func prCreateCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_pr_create", Args: args}

	if err := require(args, "branch_name", "commit_message"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	if _, err := exec.LookPath("gh"); err != nil {
		result.Err = &ToolError{ErrorCode: "gh_not_found", Message: "GitHub CLI (gh) not found. Install it to open PRs automatically.", Retryable: false}
		result.Duration = time.Since(start)
		return result
	}
	if _, err := exec.LookPath("git"); err != nil {
		result.Err = &ToolError{ErrorCode: "git_not_found", Message: "git not found on PATH", Retryable: false}
		result.Duration = time.Since(start)
		return result
	}

	branch := args["branch_name"]
	commitMsg := args["commit_message"]
	fileList := strings.Split(strings.TrimSpace(args["files"]), ",")
	cleanFiles := fileList[:0]
	for _, f := range fileList {
		f = strings.TrimSpace(f)
		if f != "" {
			cleanFiles = append(cleanFiles, f)
		}
	}

	run := func(name string, cmdArgs ...string) ([]byte, *ToolError) {
		cctx, ccancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer ccancel()
		out, err := exec.CommandContext(cctx, name, cmdArgs...).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return out, &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: false}
		}
		return out, nil
	}

	if _, terr := run("git", "checkout", "-b", branch); terr != nil {
		result.Err = terr
		result.Duration = time.Since(start)
		return result
	}
	if len(cleanFiles) > 0 {
		addArgs := append([]string{"add", "--"}, cleanFiles...)
		if _, terr := run("git", addArgs...); terr != nil {
			result.Err = terr
			result.Duration = time.Since(start)
			return result
		}
	} else {
		if _, terr := run("git", "add", "-A"); terr != nil {
			result.Err = terr
			result.Duration = time.Since(start)
			return result
		}
	}
	if _, terr := run("git", "commit", "-m", commitMsg); terr != nil {
		result.Err = terr
		result.Duration = time.Since(start)
		return result
	}
	if _, terr := run("git", "push", "-u", "origin", branch); terr != nil {
		result.Err = terr
		result.Duration = time.Since(start)
		return result
	}

	prOut, terr := run("gh", "pr", "create", "--base", "main", "--head", branch, "--title", commitMsg, "--body", "Generated by terraform-dev.")
	if terr != nil {
		result.Err = terr
		result.Duration = time.Since(start)
		return result
	}
	prURL := ""
	for _, line := range strings.Split(strings.TrimSpace(string(prOut)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			prURL = line
			break
		}
	}
	payload := map[string]any{"pr_url": prURL, "branch": branch}
	enc, _ := json.Marshal(payload)
	result.Output = json.RawMessage(enc)
	result.Duration = time.Since(start)
	return result
}

// planSummaryCall fetches the plan summary and best-effort enriches it with a
// formatted monthly cost delta extracted from `hcptf run read`. Cost lookup
// errors never fail the call — the field is simply omitted.
func planSummaryCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_plan_summary", Args: args}

	planArgs, aerr := planSummary(args)
	if aerr != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: aerr.Error()}
		result.Duration = time.Since(start)
		return result
	}

	pctx, pcancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer pcancel()
	planOut, execErr := exec.CommandContext(pctx, "hcptf", planArgs...).Output()
	if execErr != nil {
		result.Err = normalizeExecError(execErr, pctx, planOut, timeoutSec)
		result.Duration = time.Since(start)
		return result
	}
	if looksLikeHTML(string(planOut)) {
		result.Err = htmlGuardError()
		result.Duration = time.Since(start)
		return result
	}

	var planMap map[string]any
	if err := json.Unmarshal(planOut, &planMap); err != nil {
		// Passthrough if it's not a JSON object we can merge into.
		result.Output = json.RawMessage(planOut)
		result.Duration = time.Since(start)
		return result
	}

	// Best-effort cost estimate — never fail on error.
	if delta, ok := fetchCostDelta(ctx, args["run_id"], timeoutSec); ok {
		planMap["cost_estimate_monthly_delta"] = delta
	}

	enriched, mErr := json.Marshal(planMap)
	if mErr != nil {
		result.Output = json.RawMessage(planOut)
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(enriched)
	result.Duration = time.Since(start)
	return result
}

// planAnalyzeCall fetches plan, run, workspace-resource, and policy-check data
// for a given run and returns a structured risk assessment. Policy checks are
// best-effort: when no policies are attached the field is omitted rather than
// failing the call.
func planAnalyzeCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_plan_analyze", Args: args}

	if err := require(args, "org", "workspace", "run_id"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]
	runID := args["run_id"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chPlan := make(chan fetchResult, 1)
	chRun := make(chan fetchResult, 1)
	chRes := make(chan fetchResult, 1)
	chPol := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "plan", "read", "-run-id="+runID, "-output=json")
		chPlan <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "run", "read", "-id="+runID, "-output=json")
		chRun <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchWorkspaceResources(ctx, org, workspace, timeoutSec)
		chRes <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "policycheck", "list", "-run-id="+runID, "-output=json")
		chPol <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	rPlan := <-chPlan
	rRun := <-chRun
	rRes := <-chRes
	rPol := <-chPol

	if rPlan.err != nil {
		result.Err = rPlan.err
		result.Duration = time.Since(start)
		return result
	}
	if rRun.err != nil {
		result.Err = rRun.err
		result.Duration = time.Since(start)
		return result
	}
	if rRes.err != nil {
		result.Err = rRes.err
		result.Duration = time.Since(start)
		return result
	}

	plan := decodePlanCounts(rPlan.raw)
	runResources := decodeRunResources(rRun.raw)
	inventory, _ := unmarshalResources(rRes.raw)

	policy, policyAvailable := decodePolicyChecks(rPol.raw, rPol.err)

	factors, highestRisk := classifyRiskFactors(runResources, inventory)
	level := determineRiskLevel(plan, workspace, policy, policyAvailable, factors, runResources, inventory)
	recommendation, reason := recommendationFor(level, policy, policyAvailable, factors, plan)

	payload := map[string]any{
		"run_id":       runID,
		"risk_level":   level,
		"risk_factors": factors,
		"blast_radius": map[string]any{
			"total_resources_affected": plan.additions + plan.changes + plan.destructions,
			"additions":                plan.additions,
			"changes":                  plan.changes,
			"destructions":             plan.destructions,
			"highest_risk_resources":   highestRisk,
		},
		"recommendation":        recommendation,
		"recommendation_reason": reason,
	}
	if policyAvailable {
		payload["policy_checks"] = map[string]any{
			"total":           policy.total,
			"passed":          policy.passed,
			"failed":          policy.failed,
			"failed_policies": policy.failedNames,
		}
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

// fetchHCPTFJSON shells out to hcptf with the given args and returns the raw
// JSON body. Errors are normalized through the shared guard so HTML responses
// become a 404 ToolError.
func fetchHCPTFJSON(ctx context.Context, timeoutSec int, hcptfArgs ...string) ([]byte, *ToolError) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hcptf", hcptfArgs...)
	out, execErr := cmd.Output()
	if execErr != nil {
		return nil, normalizeExecError(execErr, ctx, out, timeoutSec)
	}
	if looksLikeHTML(string(out)) {
		return nil, htmlGuardError()
	}
	return out, nil
}

type planCounts struct {
	additions    int
	changes      int
	destructions int
}

// decodePlanCounts pulls add/change/destroy counts from `hcptf plan read` JSON.
// It tolerates a handful of plausible field-name variants so it works across
// hcptf versions without a brittle single-path lookup.
func decodePlanCounts(raw []byte) planCounts {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return planCounts{}
	}
	return planCounts{
		additions:    firstIntField(m, "resource_additions", "ResourceAdditions", "additions", "add"),
		changes:      firstIntField(m, "resource_changes", "ResourceChanges", "changes", "change"),
		destructions: firstIntField(m, "resource_destructions", "ResourceDestructions", "destructions", "destroy"),
	}
}

// decodeRunResources pulls the list of resources-affected-by-the-run from
// `hcptf run read` JSON. When the field is absent (older hcptf versions),
// callers fall back to workspace inventory-based classification.
func decodeRunResources(raw []byte) []runResourceEntry {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	for _, key := range []string{"resources", "Resources", "resource_changes", "ResourceChanges"} {
		if arr, ok := m[key].([]any); ok {
			out := make([]runResourceEntry, 0, len(arr))
			for _, item := range arr {
				entry := runResourceEntry{}
				if obj, ok := item.(map[string]any); ok {
					entry.address = firstStringField(obj, "address", "Address", "resource_address", "ResourceAddress")
					entry.resourceType = firstStringField(obj, "type", "Type", "resource_type", "ResourceType")
					entry.action = strings.ToLower(firstStringField(obj, "action", "Action", "change_action", "ChangeAction"))
				} else if s, ok := item.(string); ok {
					entry.address = s
					entry.resourceType = typeFromAddress(s)
				}
				if entry.resourceType == "" {
					entry.resourceType = typeFromAddress(entry.address)
				}
				if entry.address != "" || entry.resourceType != "" {
					out = append(out, entry)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// runResourceEntry is a minimal shape to drive risk classification from either
// `hcptf run read` resource data or the workspace inventory fallback.
type runResourceEntry struct {
	address      string
	resourceType string
	action       string
}

type policyCheckSummary struct {
	total       int
	passed      int
	failed      int
	failedNames []string
}

// decodePolicyChecks summarizes policy-check JSON. An empty list means no
// policies are attached — the caller omits the policy field entirely in that
// case. A 404 from the HTML guard is also treated as "unavailable" rather than
// fatal so plan analysis still returns useful data without Sentinel.
func decodePolicyChecks(raw []byte, ferr *ToolError) (policyCheckSummary, bool) {
	if ferr != nil {
		if ferr.ErrorCode == "404" {
			return policyCheckSummary{}, false
		}
		return policyCheckSummary{}, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return policyCheckSummary{}, false
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return policyCheckSummary{}, false
	}
	if len(arr) == 0 {
		return policyCheckSummary{}, false
	}
	sum := policyCheckSummary{total: len(arr), failedNames: []string{}}
	for _, item := range arr {
		status := strings.ToLower(firstStringField(item, "status", "Status", "result", "Result"))
		name := firstStringField(item, "name", "Name", "policy", "Policy", "policy_name", "PolicyName")
		if name == "" {
			name = firstStringField(item, "id", "ID")
		}
		failedHere := false
		switch status {
		case "passed", "pass", "soft_failed_override", "overridden":
			sum.passed++
		case "failed", "fail", "hard_failed", "errored":
			sum.failed++
			failedHere = true
		default:
			if n, ok := toIntAny(item["failed"]); ok && n > 0 {
				sum.failed += n
				failedHere = true
			}
			if n, ok := toIntAny(item["passed"]); ok && n > 0 {
				sum.passed += n
			}
		}
		if failedHere && name != "" {
			sum.failedNames = append(sum.failedNames, name)
		}
	}
	if sum.total == 0 {
		return policyCheckSummary{}, false
	}
	return sum, true
}

// classifyRiskFactors groups resources by semantic category (IAM, security
// group, database, networking, load balancer) so the REPL and agent can
// surface the specific reasons a plan is risky. Returns the factors plus a
// "highest risk" resource list drawn from the most severe category that fired.
func classifyRiskFactors(runResources []runResourceEntry, inventory []workspaceResource) ([]map[string]any, []string) {
	type group struct {
		factor    string
		severity  string
		resources []string
	}

	categories := []struct {
		factor     string
		severity   string
		matchType  func(string) bool
		severityN  int
	}{
		{"IAM resource modification", "High", func(t string) bool { return strings.Contains(t, "iam") }, 3},
		{"Security group change", "High", func(t string) bool { return strings.Contains(t, "security_group") }, 3},
		{"Database change", "High", func(t string) bool {
			return strings.Contains(t, "rds") || strings.Contains(t, "database") || strings.Contains(t, "_db") || strings.HasSuffix(t, "db") || strings.HasPrefix(t, "db_") || strings.Contains(t, "dynamodb") || strings.Contains(t, "documentdb")
		}, 3},
		{"Networking change", "Medium", func(t string) bool {
			return strings.Contains(t, "vpc") || strings.Contains(t, "subnet") || strings.Contains(t, "route")
		}, 2},
		{"Load balancer change", "Medium", func(t string) bool {
			return strings.Contains(t, "alb") || strings.Contains(t, "_elb") || strings.HasSuffix(t, "elb") || strings.Contains(t, "_lb") || strings.HasSuffix(t, "_lb") || strings.Contains(t, "lb_")
		}, 2},
	}

	// Collect candidate resources: prefer run-level resources; fall back to
	// workspace inventory when run data doesn't include per-resource detail.
	candidates := make([]runResourceEntry, 0, len(runResources))
	if len(runResources) > 0 {
		candidates = append(candidates, runResources...)
	} else {
		for _, r := range inventory {
			candidates = append(candidates, runResourceEntry{address: r.Address, resourceType: r.Type})
		}
	}

	buckets := make(map[string]*group, len(categories))
	order := make([]string, 0, len(categories))
	for _, c := range candidates {
		t := strings.ToLower(c.resourceType)
		if t == "" {
			continue
		}
		for _, cat := range categories {
			if !cat.matchType(t) {
				continue
			}
			g, ok := buckets[cat.factor]
			if !ok {
				g = &group{factor: cat.factor, severity: cat.severity}
				buckets[cat.factor] = g
				order = append(order, cat.factor)
			}
			addr := c.address
			if addr == "" {
				addr = c.resourceType
			}
			if !containsString(g.resources, addr) {
				g.resources = append(g.resources, addr)
			}
			break
		}
	}

	factors := make([]map[string]any, 0, len(order))
	for _, k := range order {
		g := buckets[k]
		sort.Strings(g.resources)
		factors = append(factors, map[string]any{
			"factor":    g.factor,
			"severity":  g.severity,
			"resources": g.resources,
		})
	}

	highest := []string{}
	severityRank := map[string]int{"Critical": 4, "High": 3, "Medium": 2, "Low": 1}
	bestRank := 0
	for _, f := range factors {
		sev := f["severity"].(string)
		if severityRank[sev] > bestRank {
			bestRank = severityRank[sev]
			highest = append([]string(nil), f["resources"].([]string)...)
		} else if severityRank[sev] == bestRank {
			highest = append(highest, f["resources"].([]string)...)
		}
	}
	sort.Strings(highest)
	highest = dedupeStrings(highest)
	return factors, highest
}

// determineRiskLevel applies the heuristics in the order given in the v0.7
// plan — highest wins. Policy failures trump every other signal.
func determineRiskLevel(plan planCounts, workspace string, policy policyCheckSummary, policyAvailable bool, factors []map[string]any, runResources []runResourceEntry, inventory []workspaceResource) string {
	if policyAvailable && policy.failed > 0 {
		return "Critical"
	}
	if plan.destructions > 0 && strings.Contains(strings.ToLower(workspace), "prod") {
		return "Critical"
	}
	if plan.destructions > 5 {
		return "Critical"
	}
	if plan.destructions > 0 {
		return "High"
	}
	for _, f := range factors {
		if f["severity"] == "High" {
			return "High"
		}
	}
	for _, f := range factors {
		if f["severity"] == "Medium" {
			return "Medium"
		}
	}
	if plan.changes > 10 {
		return "Medium"
	}

	// Low: only null_resource / random_ resources.
	candidates := runResources
	if len(candidates) == 0 {
		candidates = make([]runResourceEntry, 0, len(inventory))
		for _, r := range inventory {
			candidates = append(candidates, runResourceEntry{address: r.Address, resourceType: r.Type})
		}
	}
	if len(candidates) > 0 && allBenignTypes(candidates) {
		return "Low"
	}
	if plan.additions > 0 && plan.changes == 0 && plan.destructions == 0 {
		return "Low"
	}
	return "Low"
}

func allBenignTypes(entries []runResourceEntry) bool {
	for _, e := range entries {
		t := strings.ToLower(e.resourceType)
		if !strings.HasPrefix(t, "null_resource") && !strings.HasPrefix(t, "random_") {
			return false
		}
	}
	return true
}

func recommendationFor(level string, policy policyCheckSummary, policyAvailable bool, factors []map[string]any, plan planCounts) (string, string) {
	reasons := []string{}
	if policyAvailable && policy.failed > 0 {
		if len(policy.failedNames) > 0 {
			reasons = append(reasons, "Failed policies: "+strings.Join(policy.failedNames, ", "))
		} else {
			reasons = append(reasons, fmt.Sprintf("%d policy check(s) failed", policy.failed))
		}
	}
	if plan.destructions > 0 {
		reasons = append(reasons, fmt.Sprintf("%d destruction(s)", plan.destructions))
	}
	if plan.changes > 10 {
		reasons = append(reasons, fmt.Sprintf("%d in-place changes", plan.changes))
	}
	for _, f := range factors {
		reasons = append(reasons, fmt.Sprintf("%s (%s)", f["factor"], f["severity"]))
	}

	switch level {
	case "Critical":
		msg := "Do not apply — critical risk."
		if len(reasons) > 0 {
			msg += " " + strings.Join(reasons, "; ") + "."
		}
		return "do_not_apply", msg
	case "High":
		msg := "Review carefully before applying — high-risk changes detected."
		if len(reasons) > 0 {
			msg += " " + strings.Join(reasons, "; ") + "."
		}
		return "review_before_applying", msg
	case "Medium":
		msg := "Review the plan before applying."
		if len(reasons) > 0 {
			msg += " " + strings.Join(reasons, "; ") + "."
		}
		return "review_before_applying", msg
	default:
		msg := "Safe to apply — no elevated risk factors detected."
		if plan.additions+plan.changes+plan.destructions == 0 {
			msg = "No changes proposed — nothing to apply."
		}
		return "safe_to_apply", msg
	}
}

func firstIntField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n, ok := toIntAny(v); ok {
				return n
			}
		}
	}
	return 0
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func toIntAny(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		var x int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &x); err == nil {
			return x, true
		}
	}
	return 0, false
}

func typeFromAddress(addr string) string {
	if addr == "" {
		return ""
	}
	// Strip module prefix like `module.foo.`.
	for strings.HasPrefix(addr, "module.") {
		parts := strings.SplitN(addr, ".", 3)
		if len(parts) < 3 {
			return ""
		}
		addr = parts[2]
	}
	// "aws_iam_role.app" → "aws_iam_role"
	if dot := strings.IndexByte(addr, '.'); dot > 0 {
		return addr[:dot]
	}
	return addr
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func dedupeStrings(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	out := xs[:0]
	seen := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

// normalizeExecError turns an exec.ExitError into the shared ToolError shape,
// matching the pattern used throughout callDispatch.
func normalizeExecError(execErr error, ctx context.Context, stdout []byte, timeoutSec int) *ToolError {
	retryable := false
	msg := execErr.Error()
	stderr := ""
	if e, ok := execErr.(*exec.ExitError); ok {
		stderr = strings.TrimSpace(string(e.Stderr))
		if stderr != "" {
			msg = stderr
		}
		retryable = e.ExitCode() == 1
	}
	if ctx.Err() != nil {
		msg = fmt.Sprintf("tool timed out after %ds", timeoutSec)
		retryable = true
	}
	if looksLikeHTML(string(stdout)) || looksLikeHTML(stderr) {
		return htmlGuardError()
	}
	return &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: retryable}
}

func fetchCostDelta(ctx context.Context, runID string, timeoutSec int) (string, bool) {
	if runID == "" {
		return "", false
	}
	rctx, rcancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer rcancel()
	out, err := exec.CommandContext(rctx, "hcptf", "run", "read", "-id="+runID, "-output=json").Output()
	if err != nil || looksLikeHTML(string(out)) {
		return "", false
	}
	var runMap map[string]any
	if jerr := json.Unmarshal(out, &runMap); jerr != nil {
		return "", false
	}
	ce, ok := runMap["cost_estimate"].(map[string]any)
	if !ok {
		return "", false
	}
	raw, ok := ce["delta_monthly_cost"]
	if !ok {
		return "", false
	}
	var num float64
	switch v := raw.(type) {
	case float64:
		num = v
	case string:
		if v == "" {
			return "", false
		}
		if _, serr := fmt.Sscanf(v, "%f", &num); serr != nil {
			return "", false
		}
	default:
		return "", false
	}
	if num >= 0 {
		return fmt.Sprintf("+$%.2f", num), true
	}
	return fmt.Sprintf("-$%.2f", -num), true
}

// writeAuditLog appends a single JSON line per tool invocation to
// ~/.terraform-dev/audit.log. Logging failures are reported to stderr and
// never bubble up — they must not block tool execution.
func writeAuditLog(name string, args map[string]string, result *CallResult) {
	path, err := auditLogPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit log: resolve path: %v\n", err)
		return
	}

	status := "success"
	if result.Err != nil {
		status = "error"
	}

	entry := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"tool":      name,
		"args":      args,
		"result":    status,
		"user":      hcptfWhoAmI(),
	}
	if result.Err != nil {
		entry["error_code"] = result.Err.ErrorCode
	}

	line, mErr := json.Marshal(entry)
	if mErr != nil {
		fmt.Fprintf(os.Stderr, "audit log: marshal: %v\n", mErr)
		return
	}

	f, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if oerr != nil {
		fmt.Fprintf(os.Stderr, "audit log: open: %v\n", oerr)
		return
	}
	defer f.Close()

	if _, werr := f.Write(append(line, '\n')); werr != nil {
		fmt.Fprintf(os.Stderr, "audit log: write: %v\n", werr)
	}
}

func auditLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".terraform-dev")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.log"), nil
}

var (
	whoAmIOnce sync.Once
	whoAmIVal  string
)

func hcptfWhoAmI() string {
	whoAmIOnce.Do(func() {
		out, err := exec.Command("hcptf", "whoami", "-output=json").Output()
		if err != nil {
			whoAmIVal = "unknown"
			return
		}
		var m map[string]any
		if json.Unmarshal(out, &m) == nil {
			for _, key := range []string{"Username", "username", "Email", "email", "Name", "name"} {
				if s, ok := m[key].(string); ok && s != "" {
					whoAmIVal = s
					return
				}
			}
		}
		whoAmIVal = strings.TrimSpace(string(out))
		if whoAmIVal == "" {
			whoAmIVal = "unknown"
		}
	})
	return whoAmIVal
}

func callDispatch(ctx context.Context, name string, args map[string]string, timeoutSec int) *CallResult {
	if name == "_hcp_tf_workspace_diff" {
		return workspaceDiffCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspace_describe" {
		return workspaceDescribeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_variable_diff" {
		return variableDiffCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_plan_summary" {
		return planSummaryCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_plan_analyze" {
		return planAnalyzeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_config_validate" {
		return configValidateCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_pr_create" {
		return prCreateCall(ctx, args, timeoutSec)
	}

	start := time.Now()
	result := &CallResult{ToolName: name, Args: args}

	cmdArgs, err := buildArgs(name, args)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", cmdArgs...)
	out, execErr := cmd.Output()
	result.Duration = time.Since(start)

	if execErr != nil {
		var exitErr *exec.ExitError
		retryable := false
		msg := execErr.Error()
		stderr := ""
		if e, ok := execErr.(*exec.ExitError); ok {
			exitErr = e
			stderr = strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				msg = stderr
			}
			retryable = exitErr.ExitCode() == 1
		}
		if ctx.Err() != nil {
			msg = fmt.Sprintf("tool timed out after %ds", timeoutSec)
			retryable = true
		}
		if looksLikeHTML(string(out)) || looksLikeHTML(stderr) {
			result.Err = htmlGuardError()
			return result
		}
		result.Err = &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: retryable}
		return result
	}

	if looksLikeHTML(string(out)) {
		result.Err = htmlGuardError()
		return result
	}

	result.Output = json.RawMessage(out)
	return result
}

func looksLikeHTML(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype") ||
		strings.HasPrefix(lower, "<html") ||
		strings.Contains(lower, "<!doctype") ||
		strings.Contains(lower, "<html")
}

func htmlGuardError() *ToolError {
	return &ToolError{
		ErrorCode: "404",
		Message:   "Resource not available or requires a higher plan tier.",
		Retryable: false,
	}
}

func buildArgs(toolName string, args map[string]string) ([]string, error) {
	switch toolName {
	case "_hcp_tf_runs_list_recent":
		return runsListRecent(args)
	case "_hcp_tf_drift_detect":
		return driftDetect(args)
	case "_hcp_tf_policy_check":
		return policyCheck(args)
	case "_hcp_tf_plan_summary":
		return planSummary(args)
	case "_hcp_tf_run_create":
		return runCreate(args)
	case "_hcp_tf_run_apply":
		return runApply(args)
	case "_hcp_tf_run_discard":
		return runDiscard(args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", toolName)
	}
}

func require(args map[string]string, keys ...string) error {
	for _, k := range keys {
		if args[k] == "" {
			return fmt.Errorf("missing required argument: %s", k)
		}
	}
	return nil
}

func runsListRecent(args map[string]string) ([]string, error) {
	if err := require(args, "org", "workspace"); err != nil {
		return nil, err
	}
	return []string{"run", "list",
		"-org=" + args["org"],
		"-workspace=" + args["workspace"],
		"-output=json",
	}, nil
}

func driftDetect(args map[string]string) ([]string, error) {
	if err := require(args, "org", "workspace"); err != nil {
		return nil, err
	}
	return []string{"assessmentresult", "list",
		"-org=" + args["org"],
		"-name=" + args["workspace"],
		"-output=json",
	}, nil
}

func policyCheck(args map[string]string) ([]string, error) {
	if err := require(args, "run_id"); err != nil {
		return nil, err
	}
	return []string{"policycheck", "list",
		"-run-id=" + args["run_id"],
		"-output=json",
	}, nil
}

func planSummary(args map[string]string) ([]string, error) {
	if err := require(args, "run_id"); err != nil {
		return nil, err
	}
	return []string{"plan", "read",
		"-run-id=" + args["run_id"],
		"-output=json",
	}, nil
}

func runCreate(args map[string]string) ([]string, error) {
	if err := require(args, "org", "workspace"); err != nil {
		return nil, err
	}
	cmd := []string{"run", "create",
		"-org=" + args["org"],
		"-workspace=" + args["workspace"],
	}
	if msg := args["message"]; msg != "" {
		cmd = append(cmd, "-message="+msg)
	}
	cmd = append(cmd, "-output=json")
	return cmd, nil
}

func runApply(args map[string]string) ([]string, error) {
	if err := require(args, "run_id", "comment"); err != nil {
		return nil, err
	}
	return []string{"run", "apply",
		"-id=" + args["run_id"],
		"-comment=" + args["comment"],
	}, nil
}

func runDiscard(args map[string]string) ([]string, error) {
	if err := require(args, "run_id", "comment"); err != nil {
		return nil, err
	}
	return []string{"run", "discard",
		"-id=" + args["run_id"],
		"-comment=" + args["comment"],
	}, nil
}

func workspaceDiffCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspace_diff", Args: args}

	if err := require(args, "org", "workspace_a", "workspace_b"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	orgA := args["org"]
	orgB := args["org_b"]
	if orgB == "" {
		orgB = orgA
	}
	wsA := args["workspace_a"]
	wsB := args["workspace_b"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chA := make(chan fetchResult, 1)
	chB := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		raw, ferr := fetchWorkspaceResources(ctx, orgA, wsA, timeoutSec)
		if ferr != nil {
			ferr.Message = "workspace_a: " + ferr.Message
		}
		chA <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchWorkspaceResources(ctx, orgB, wsB, timeoutSec)
		if ferr != nil {
			ferr.Message = "workspace_b: " + ferr.Message
		}
		chB <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	ra := <-chA
	rb := <-chB

	if ra.err != nil {
		result.Err = ra.err
		result.Duration = time.Since(start)
		return result
	}
	if rb.err != nil {
		result.Err = rb.err
		result.Duration = time.Since(start)
		return result
	}

	addrsA, err := parseResourceAddresses(ra.raw)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: "workspace_a: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	addrsB, err := parseResourceAddresses(rb.raw)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: "workspace_b: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	setA := make(map[string]struct{}, len(addrsA))
	for _, a := range addrsA {
		setA[a] = struct{}{}
	}
	setB := make(map[string]struct{}, len(addrsB))
	for _, a := range addrsB {
		setB[a] = struct{}{}
	}

	missingInB := []string{}
	missingInA := []string{}
	presentInBoth := []string{}
	for a := range setA {
		if _, ok := setB[a]; ok {
			presentInBoth = append(presentInBoth, a)
		} else {
			missingInB = append(missingInB, a)
		}
	}
	for b := range setB {
		if _, ok := setA[b]; !ok {
			missingInA = append(missingInA, b)
		}
	}
	sort.Strings(missingInB)
	sort.Strings(missingInA)
	sort.Strings(presentInBoth)

	diff := map[string]any{
		"missing_in_b":               missingInB,
		"missing_in_a":               missingInA,
		"present_in_both":            presentInBoth,
		"workspace_a_resource_count": len(addrsA),
		"workspace_b_resource_count": len(addrsB),
	}
	out, mErr := json.Marshal(diff)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// fetchWorkspaceResources shells out to `hcptf workspace resource list` and
// returns the raw JSON array of {Address, ID, Module, Provider, Type} objects.
func fetchWorkspaceResources(ctx context.Context, org, workspace string, timeoutSec int) ([]byte, *ToolError) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", "workspace", "resource", "list",
		"-org="+org,
		"-workspace="+workspace,
		"-output=json",
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		retryable := false
		msg := execErr.Error()
		stderr := ""
		if e, ok := execErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(e.Stderr))
			if stderr != "" {
				msg = stderr
			}
			retryable = e.ExitCode() == 1
		}
		if ctx.Err() != nil {
			msg = fmt.Sprintf("tool timed out after %ds", timeoutSec)
			retryable = true
		}
		if looksLikeHTML(string(out)) || looksLikeHTML(stderr) {
			return nil, htmlGuardError()
		}
		return nil, &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: retryable}
	}
	if looksLikeHTML(string(out)) {
		return nil, htmlGuardError()
	}
	return out, nil
}

// workspaceResource matches the JSON shape of `hcptf workspace resource list`.
type workspaceResource struct {
	Address  string `json:"Address"`
	ID       string `json:"ID"`
	Module   string `json:"Module"`
	Provider string `json:"Provider"`
	Type     string `json:"Type"`
}

// parseResourceAddresses extracts the Address field from each resource.
func parseResourceAddresses(raw []byte) ([]string, error) {
	items, err := unmarshalResources(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Address != "" {
			out = append(out, it.Address)
		}
	}
	return out, nil
}

// resourceTypesFromRaw returns the distinct Type values (sorted) from a
// resource list payload.
func resourceTypesFromRaw(raw []byte) ([]string, error) {
	items, err := unmarshalResources(raw)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it.Type != "" {
			seen[it.Type] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func unmarshalResources(raw []byte) ([]workspaceResource, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var items []workspaceResource
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse workspace resources: %w", err)
	}
	return items, nil
}

// workspaceDescribeCall fires `workspace read` + `workspace resource list` in
// parallel and merges them so the agent sees workspace metadata alongside the
// actual resource inventory (types + count), not just a header.
func workspaceDescribeCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspace_describe", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chMeta := make(chan fetchResult, 1)
	chRes := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		raw, ferr := fetchWorkspaceRead(ctx, org, workspace, timeoutSec)
		chMeta <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchWorkspaceResources(ctx, org, workspace, timeoutSec)
		chRes <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	rMeta := <-chMeta
	rRes := <-chRes

	if rMeta.err != nil {
		result.Err = rMeta.err
		result.Duration = time.Since(start)
		return result
	}
	if rRes.err != nil {
		result.Err = rRes.err
		result.Duration = time.Since(start)
		return result
	}

	items, perr := unmarshalResources(rRes.raw)
	if perr != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: perr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	types, _ := resourceTypesFromRaw(rRes.raw)

	merged := map[string]any{
		"workspace":      json.RawMessage(rMeta.raw),
		"resources":      json.RawMessage(rRes.raw),
		"resource_types": types,
		"resource_count": len(items),
	}
	out, mErr := json.Marshal(merged)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// fetchWorkspaceRead shells out to `hcptf workspace read` and returns the raw
// JSON body. Errors are normalized the same way fetchWorkspaceResources does.
func fetchWorkspaceRead(ctx context.Context, org, workspace string, timeoutSec int) ([]byte, *ToolError) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", "workspace", "read",
		"-org="+org,
		"-name="+workspace,
		"-output=json",
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		retryable := false
		msg := execErr.Error()
		stderr := ""
		if e, ok := execErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(e.Stderr))
			if stderr != "" {
				msg = stderr
			}
			retryable = e.ExitCode() == 1
		}
		if ctx.Err() != nil {
			msg = fmt.Sprintf("tool timed out after %ds", timeoutSec)
			retryable = true
		}
		if looksLikeHTML(string(out)) || looksLikeHTML(stderr) {
			return nil, htmlGuardError()
		}
		return nil, &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: retryable}
	}
	if looksLikeHTML(string(out)) {
		return nil, htmlGuardError()
	}
	return out, nil
}

// variableDiffCall fetches variables from two workspaces in parallel and
// returns a structured key-level diff. Values are never included — sensitive
// variables only expose the sensitive flag.
func variableDiffCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_variable_diff", Args: args}

	if err := require(args, "org", "workspace_a", "workspace_b"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	wsA := args["workspace_a"]
	wsB := args["workspace_b"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chA := make(chan fetchResult, 1)
	chB := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		raw, ferr := fetchVariables(ctx, org, wsA, timeoutSec)
		if ferr != nil {
			ferr.Message = "workspace_a: " + ferr.Message
		}
		chA <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchVariables(ctx, org, wsB, timeoutSec)
		if ferr != nil {
			ferr.Message = "workspace_b: " + ferr.Message
		}
		chB <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	ra := <-chA
	rb := <-chB

	if ra.err != nil {
		result.Err = ra.err
		result.Duration = time.Since(start)
		return result
	}
	if rb.err != nil {
		result.Err = rb.err
		result.Duration = time.Since(start)
		return result
	}

	varsA, err := parseVariables(ra.raw)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: "workspace_a: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	varsB, err := parseVariables(rb.raw)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: "workspace_b: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	mapA := make(map[string]variableEntry, len(varsA))
	for _, v := range varsA {
		mapA[v.Key] = v
	}
	mapB := make(map[string]variableEntry, len(varsB))
	for _, v := range varsB {
		mapB[v.Key] = v
	}

	onlyInA := []map[string]any{}
	onlyInB := []map[string]any{}
	inBoth := []map[string]any{}

	keysA := make([]string, 0, len(mapA))
	for k := range mapA {
		keysA = append(keysA, k)
	}
	sort.Strings(keysA)
	for _, k := range keysA {
		v := mapA[k]
		if _, ok := mapB[k]; ok {
			sensitive := v.Sensitive
			if b, ok := mapB[k]; ok && b.Sensitive {
				sensitive = true
			}
			inBoth = append(inBoth, map[string]any{
				"key":       v.Key,
				"sensitive": sensitive,
			})
		} else {
			onlyInA = append(onlyInA, map[string]any{
				"key":       v.Key,
				"category":  v.Category,
				"sensitive": v.Sensitive,
			})
		}
	}

	keysB := make([]string, 0, len(mapB))
	for k := range mapB {
		keysB = append(keysB, k)
	}
	sort.Strings(keysB)
	for _, k := range keysB {
		v := mapB[k]
		if _, ok := mapA[k]; ok {
			continue
		}
		onlyInB = append(onlyInB, map[string]any{
			"key":       v.Key,
			"category":  v.Category,
			"sensitive": v.Sensitive,
		})
	}

	diff := map[string]any{
		"only_in_a":         onlyInA,
		"only_in_b":         onlyInB,
		"in_both":           inBoth,
		"workspace_a_count": len(varsA),
		"workspace_b_count": len(varsB),
	}
	out, mErr := json.Marshal(diff)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// fetchVariables shells out to `hcptf variable list` and returns the raw JSON
// array. HTML responses are normalized through the shared guard.
func fetchVariables(ctx context.Context, org, workspace string, timeoutSec int) ([]byte, *ToolError) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", "variable", "list",
		"-org="+org,
		"-workspace="+workspace,
		"-output=json",
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		retryable := false
		msg := execErr.Error()
		stderr := ""
		if e, ok := execErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(e.Stderr))
			if stderr != "" {
				msg = stderr
			}
			retryable = e.ExitCode() == 1
		}
		if ctx.Err() != nil {
			msg = fmt.Sprintf("tool timed out after %ds", timeoutSec)
			retryable = true
		}
		if looksLikeHTML(string(out)) || looksLikeHTML(stderr) {
			return nil, htmlGuardError()
		}
		return nil, &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: retryable}
	}
	if looksLikeHTML(string(out)) {
		return nil, htmlGuardError()
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "No variables found" {
		return []byte("[]"), nil
	}
	return out, nil
}

// variableEntry is the subset of `hcptf variable list` fields the diff needs —
// Value is intentionally ignored so sensitive values cannot leak into output.
type variableEntry struct {
	Key       string
	Category  string
	Sensitive bool
}

// parseVariables unmarshals the hcptf JSON array (where Sensitive comes back as
// the string "true"/"false") and returns the value-free projection.
func parseVariables(raw []byte) ([]variableEntry, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var items []struct {
		Key       string `json:"Key"`
		Category  string `json:"Category"`
		Sensitive string `json:"Sensitive"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse variables: %w", err)
	}
	out := make([]variableEntry, 0, len(items))
	for _, it := range items {
		if it.Key == "" {
			continue
		}
		out = append(out, variableEntry{
			Key:       it.Key,
			Category:  it.Category,
			Sensitive: strings.EqualFold(strings.TrimSpace(it.Sensitive), "true"),
		})
	}
	return out, nil
}

// Definitions returns the tool definitions for the Anthropic tool_use API.
func Definitions() []ToolDef {
	return []ToolDef{
		{
			Name:        "_hcp_tf_runs_list_recent",
			Description: "Lists the most recent runs for a workspace with status, timestamps, resource counts, and cost delta.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
				},
				"required": []string{"org", "workspace"},
			},
		},
		{
			Name:        "_hcp_tf_workspace_diff",
			Description: "Compares two HCP Terraform workspaces by fetching each workspace's state in parallel and returning a structured resource-address diff: missing_in_a, missing_in_b, present_in_both, plus per-workspace resource counts.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":         map[string]any{"type": "string", "description": "HCP Terraform organization name (also used for workspace_b unless org_b is provided)"},
					"workspace_a": map[string]any{"type": "string", "description": "First workspace name"},
					"workspace_b": map[string]any{"type": "string", "description": "Second workspace name"},
					"org_b":       map[string]any{"type": "string", "description": "Optional organization for workspace_b when diffing across orgs; defaults to org"},
				},
				"required": []string{"org", "workspace_a", "workspace_b"},
			},
		},
		{
			Name:        "_hcp_tf_workspace_describe",
			Description: "Returns workspace metadata merged with the actual resource inventory: workspace read fields under `workspace`, the full resource list under `resources`, distinct `resource_types`, and a total `resource_count`.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
				},
				"required": []string{"org", "workspace"},
			},
		},
		{
			Name:        "_hcp_tf_variable_diff",
			Description: "Compares variables between two HCP Terraform workspaces in the same organization, fetching both in parallel. Returns key-level diff with only_in_a, only_in_b, in_both, and per-workspace counts. Never exposes variable values — sensitive variables are flagged with sensitive:true only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":         map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace_a": map[string]any{"type": "string", "description": "First workspace name"},
					"workspace_b": map[string]any{"type": "string", "description": "Second workspace name"},
				},
				"required": []string{"org", "workspace_a", "workspace_b"},
			},
		},
		{
			Name:        "_hcp_tf_drift_detect",
			Description: "Returns assessment results for a workspace showing detected drift and changed resources.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
				},
				"required": []string{"org", "workspace"},
			},
		},
		{
			Name:        "_hcp_tf_policy_check",
			Description: "Returns policy check results for a run: which checks passed/failed, which rules fired.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string", "description": "Run ID (run-xxx)"},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "_hcp_tf_plan_summary",
			Description: "Returns a summary of a plan: adds/changes/destroys, flagged risks, and when available a monthly cost delta.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string", "description": "Run ID (run-xxx)"},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "_hcp_tf_plan_analyze",
			Description: "Produces a structured risk assessment for a run: risk level (Low|Medium|High|Critical), specific risk factors with severity and affected resources, blast radius (adds/changes/destroys plus highest-risk resources), optional policy-check results when policies are attached, and a recommendation (safe_to_apply|review_before_applying|do_not_apply) with plain-English reasoning. Read-only; safe to call in readonly and apply modes.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
					"run_id":    map[string]any{"type": "string", "description": "Run ID (run-xxx)"},
				},
				"required": []string{"org", "workspace", "run_id"},
			},
		},
		{
			Name:        "_hcp_tf_run_create",
			Description: "Creates a new run in a workspace and returns the run_id. This is a mutating operation — only available when --apply is set. The caller is responsible for obtaining explicit user approval before calling this tool.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
					"message":   map[string]any{"type": "string", "description": "Optional message describing the run"},
				},
				"required": []string{"org", "workspace"},
			},
		},
		{
			Name:        "_hcp_tf_run_apply",
			Description: "Applies a previously-created run in a workspace. This triggers real infrastructure changes and is the only tool that causes an apply. Only available when --apply is set. The caller is responsible for obtaining explicit user approval and for showing the plan summary before calling this tool.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name"},
					"run_id":    map[string]any{"type": "string", "description": "Run ID (run-xxx) to apply"},
					"comment":   map[string]any{"type": "string", "description": "Comment recorded on the apply"},
				},
				"required": []string{"org", "workspace", "run_id", "comment"},
			},
		},
		{
			Name:        "_hcp_tf_run_discard",
			Description: "Discards a pending run so it cannot be applied. Only available when --apply is set. Use this to cancel a run the user no longer wants to proceed with.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id":  map[string]any{"type": "string", "description": "Run ID (run-xxx) to discard"},
					"comment": map[string]any{"type": "string", "description": "Comment recorded on the discard"},
				},
				"required": []string{"run_id", "comment"},
			},
		},
		{
			Name:        "_hcp_tf_config_validate",
			Description: "Runs `terraform validate` against a local directory containing .tf files and returns { valid, errors } describing any HCL or schema issues. A best-effort `terraform init -backend=false` is run first when providers are not yet installed. Returns { error_code: terraform_not_found } when the terraform CLI is unavailable.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"config_path": map[string]any{"type": "string", "description": "Absolute or working-directory-relative path to the directory containing .tf files to validate."},
				},
				"required": []string{"config_path"},
			},
		},
		{
			Name:        "_hcp_tf_pr_create",
			Description: "Creates a new branch from HEAD, commits the specified files, pushes to origin, and opens a GitHub pull request against main using the gh CLI. Returns { pr_url, branch }. Returns { error_code: gh_not_found } when the GitHub CLI is not installed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":            map[string]any{"type": "string", "description": "HCP Terraform organization name (optional, informational)"},
					"workspace":      map[string]any{"type": "string", "description": "HCP Terraform workspace name (optional, informational)"},
					"branch_name":    map[string]any{"type": "string", "description": "Name of the branch to create and push"},
					"commit_message": map[string]any{"type": "string", "description": "Commit subject, also used as the PR title"},
					"files":          map[string]any{"type": "string", "description": "Comma-separated list of file paths to include in the commit (relative to the current directory). If empty, `git add -A` is used."},
				},
				"required": []string{"branch_name", "commit_message"},
			},
		},
	}
}

// DefinitionsFor returns tool definitions filtered for the given mode. When
// readonly is true the mutating tools are excluded so the model never sees
// them.
func DefinitionsFor(readonly bool) []ToolDef {
	all := Definitions()
	if !readonly {
		return all
	}
	out := make([]ToolDef, 0, len(all))
	for _, d := range all {
		if MutatingTools[d.Name] {
			continue
		}
		out = append(out, d)
	}
	return out
}

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}
