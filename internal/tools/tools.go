package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MutatingTools is the set of tool names that cause a state change in HCP
// Terraform. The REPL approval gate and the readonly-mode filter key off this
// set.
var MutatingTools = map[string]bool{
	"_hcp_tf_run_create":         true,
	"_hcp_tf_run_apply":          true,
	"_hcp_tf_run_discard":        true,
	"_hcp_tf_workspace_create":   true,
	"_hcp_tf_workspace_populate": true,
	"_hcp_tf_upgrade_preview":    true,
	"_hcp_tf_rollback":           true,
	"_hcp_tf_version_upgrade":    true,
	"_hcp_tf_batch_upgrade":      true,
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

	prOut, terr := run("gh", "pr", "create", "--base", "main", "--head", branch, "--title", commitMsg, "--body", "Generated by tfpilot.")
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
	chCost := make(chan *costEstimate, 1)

	var wg sync.WaitGroup
	wg.Add(5)
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
	go func() {
		defer wg.Done()
		chCost <- fetchCostEstimate(ctx, runID, timeoutSec)
	}()
	wg.Wait()
	rPlan := <-chPlan
	rRun := <-chRun
	rRes := <-chRes
	rPol := <-chPol
	rCost := <-chCost

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
	if reduction := riskReductionSuggestions(level, factors, plan); len(reduction) > 0 {
		payload["how_to_reduce_risk"] = reduction
	}
	if rCost != nil {
		payload["cost_estimate_available"] = true
		payload["cost_estimate"] = map[string]any{
			"prior_monthly_cost":    rCost.Prior,
			"proposed_monthly_cost": rCost.Proposed,
			"delta_monthly_cost":    rCost.Delta,
			"delta_sign":            rCost.DeltaSign,
			"summary":               rCost.Summary,
		}
	} else {
		payload["cost_estimate_available"] = false
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

// runDiagnoseCall fetches run details plus plan and apply logs for a failed
// run, categorizes the root cause, and returns a structured diagnosis with a
// suggested fix. Apply logs are optional: when the run errored before the
// apply phase, the apply-logs fetch will 404 and we degrade gracefully.
func runDiagnoseCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_run_diagnose", Args: args}

	if err := require(args, "org", "workspace", "run_id"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	runID := args["run_id"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chRun := make(chan fetchResult, 1)
	chPlanLogs := make(chan fetchResult, 1)
	chApplyLogs := make(chan fetchResult, 1)
	chPolicy := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "run", "show", "-id="+runID, "-output=json")
		chRun <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "plan", "logs", "-run-id="+runID, "-output=json")
		chPlanLogs <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "apply", "logs", "-run-id="+runID, "-output=json")
		chApplyLogs <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "policycheck", "list", "-run-id="+runID, "-output=json")
		chPolicy <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	rRun := <-chRun
	rPlanLogs := <-chPlanLogs
	rApplyLogs := <-chApplyLogs
	rPolicy := <-chPolicy

	if rRun.err != nil {
		result.Err = rRun.err
		result.Duration = time.Since(start)
		return result
	}

	status := decodeRunStatus(rRun.raw)
	planLogs := decodeLogsField(rPlanLogs.raw)
	applyLogs := decodeLogsField(rApplyLogs.raw)

	diag := categorizeFailure(planLogs, applyLogs)

	payload := map[string]any{
		"run_id":             runID,
		"status":             status,
		"error_category":     diag.category,
		"error_summary":      diag.summary,
		"error_detail":       diag.detail,
		"affected_resources": diag.resources,
		"log_snippet":        diag.snippet,
		"suggested_fix":      diag.fix,
	}

	if diag.category == "policy" && rPolicy.err == nil {
		if interpreted := interpretFailedPolicies(rPolicy.raw); len(interpreted) > 0 {
			payload["failed_policies"] = interpreted
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

// FetchRunStatus reads the run's current status via `hcptf run read`. The
// REPL's apply gate uses this to short-circuit when the run is already
// finalized (e.g., planned_and_finished) — applying such a run would return
// a "transition not allowed" error from HCP Terraform.
func FetchRunStatus(ctx context.Context, runID string, timeoutSec int) (string, *ToolError) {
	if runID == "" {
		return "", &ToolError{ErrorCode: "invalid_tool", Message: "run_id is required"}
	}
	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "run", "read", "-id="+runID, "-output=json")
	if ferr != nil {
		return "", ferr
	}
	return decodeRunStatus(raw), nil
}

// decodeRunStatus pulls the status field out of a `hcptf run read` JSON blob.
// Returns empty string when the payload is missing or the field is absent.
func decodeRunStatus(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return firstStringField(m, "Status", "status")
}

// decodeLogsField pulls the "logs" string from a `hcptf {plan,apply} logs
// -output=json` blob. Returns empty string when the payload is missing or
// unparseable, so callers can degrade gracefully when one phase is absent.
func decodeLogsField(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if s, ok := m["logs"].(string); ok {
		return s
	}
	if s, ok := m["Logs"].(string); ok {
		return s
	}
	return ""
}

// diagnosis is the internal result of categorizeFailure. It is flattened into
// the tool's JSON payload by runDiagnoseCall.
type diagnosis struct {
	category  string
	summary   string
	detail    string
	fix       string
	snippet   string
	resources []string
}

// categoryPlaybook holds the stable prose we return for each error_category.
// The summary describes what went wrong in one sentence; the fix describes the
// next action the user should take. Detail is filled in from the matched log
// line at runtime.
var categoryPlaybook = map[string]struct {
	summary string
	fix     string
}{
	"policy": {
		summary: "The run was blocked by a policy check.",
		fix:     "Call _hcp_tf_policy_check to see which policies failed, then update the configuration (or request a policy override) so the failing rules pass.",
	},
	"quota": {
		summary: "The cloud provider rejected the change because an account limit or service quota was exceeded.",
		fix:     "Request a quota increase from the cloud provider, or remove unused resources before re-running.",
	},
	"auth": {
		summary: "The run failed because the configured cloud credentials are not authorized to perform the requested action.",
		fix:     "Check the cloud credentials (AWS / Azure / GCP) configured as workspace variables. The principal is missing permission for the action above — add it to the IAM role/policy or switch to a principal that has it.",
	},
	"resource_conflict": {
		summary: "A resource the plan wanted to create already exists in the target account.",
		fix:     "Import the existing resource into Terraform state, rename the new resource, or remove the conflicting one before re-running.",
	},
	"network": {
		summary: "The run failed on a network call — the provider could not reach its API endpoint.",
		fix:     "Retry the run. If it keeps failing, check VPC endpoints, outbound network access from the HCP Terraform agent, and any firewall rules.",
	},
	"provider": {
		summary: "The Terraform provider itself failed or could not be initialized.",
		fix:     "Check the provider version pinned in the configuration and the provider's release notes for known issues. A `terraform init -upgrade` followed by a fresh run often clears transient provider install failures.",
	},
	"config": {
		summary: "The plan failed because the Terraform configuration is invalid.",
		fix:     "Fix the configuration error highlighted in the log snippet and re-run. Running _hcp_tf_config_validate against the local directory can surface the issue before you push again.",
	},
	"unknown": {
		summary: "The run failed, but the error did not match any known category.",
		fix:     "Review the log snippet below and the full run logs in HCP Terraform for context.",
	},
}

// categoryKeywords defines the ordered keyword match list for categorizing a
// failure. Order matters: policy is checked first because a policy-violation
// line can contain words like "denied" that would otherwise match auth, and
// quota is checked before auth because "LimitExceeded" has no auth overlap but
// "AccessDenied" is unambiguously auth.
var categoryKeywords = []struct {
	category string
	keywords []string
}{
	{"policy", []string{"policy check failed", "sentinel", "policy violation", " opa ", "opa policy"}},
	{"quota", []string{"limitexceeded", "servicequotaexceeded", "quota exceeded", "limit exceeded"}},
	{"auth", []string{"accessdenied", "unauthorized", "not authorized", " 403 ", "invalidclienttokenid", "signaturedoesnotmatch", "invalid credentials", "expiredtoken", "expiredtokenexception", "requestexpired", "tokenrefreshrequired"}},
	{"resource_conflict", []string{"alreadyexists", "already exists", "bucketalreadyownedbyyou", "duplicate resource", "conflict:"}},
	{"network", []string{"dial tcp", "connection refused", "no route to host", "i/o timeout", "context deadline exceeded", "tls handshake"}},
	{"provider", []string{"provider produced", "protorpc", "incompatible provider", "failed to install provider", "plugin did not respond"}},
	{"config", []string{"unsupported argument", "unknown field", "syntax error", "parse error", "invalid configuration", "error: invalid"}},
}

// categorizeFailure scans plan and apply log content and returns a diagnosis.
// Pure function — no I/O — so it can be table-tested in isolation.
func categorizeFailure(planLogs, applyLogs string) diagnosis {
	// Favor apply logs for the snippet when apply ran, but scan both streams
	// for category keywords so we don't miss a policy block that happens in
	// the plan phase when apply also produced noise.
	primary := applyLogs
	if strings.TrimSpace(primary) == "" {
		primary = planLogs
	}
	combined := planLogs
	if applyLogs != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += applyLogs
	}

	lines := splitLogLines(combined)
	primaryLines := splitLogLines(primary)

	category := "unknown"
	var matched string
	for _, bucket := range categoryKeywords {
		if line, ok := findFirstMatch(lines, bucket.keywords); ok {
			category = bucket.category
			matched = line
			break
		}
	}

	play := categoryPlaybook[category]

	d := diagnosis{
		category: category,
		summary:  play.summary,
		fix:      play.fix,
	}
	if matched != "" {
		d.detail = trimLine(matched)
	}

	d.snippet = buildSnippet(primaryLines, matched, 5)
	// Resource addresses rarely appear on the error line itself — the provider
	// error is usually a separate line from the "Plan to create" / "Creating"
	// line that names the resource. Scan the tail of the combined log so we
	// catch the resource that was in-flight when the error fired.
	d.resources = extractResources(tailLines(lines, 20), matched)
	return d
}

// tailLines returns the last n lines, joined by newline. Used to widen the
// search window for resource address extraction beyond the single matched
// line.
func tailLines(lines []string, n int) string {
	if len(lines) == 0 {
		return ""
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
}

var logLineSep = regexp.MustCompile(`\r?\n`)

func splitLogLines(s string) []string {
	if s == "" {
		return nil
	}
	raw := logLineSep.Split(s, -1)
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// findFirstMatch returns the first log line that contains any of the given
// keywords (case-insensitive). The search is ordered by line so we report the
// earliest occurrence of the error.
func findFirstMatch(lines []string, keywords []string) (string, bool) {
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return line, true
			}
		}
	}
	return "", false
}

// buildSnippet returns up to n log lines of context. When a keyword line was
// matched we anchor on that line; otherwise we fall back to the tail of the
// log which is where Terraform prints its final error block.
func buildSnippet(lines []string, matched string, n int) string {
	if len(lines) == 0 {
		return ""
	}
	end := len(lines)
	idx := -1
	if matched != "" {
		for i, line := range lines {
			if line == matched {
				idx = i
				break
			}
		}
	}
	if idx >= 0 {
		end = idx + 1
		if end > len(lines) {
			end = len(lines)
		}
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	picked := lines[start:end]
	out := make([]string, 0, len(picked))
	for _, line := range picked {
		out = append(out, trimLine(line))
	}
	return strings.Join(out, "\n")
}

var jsonMessageRe = regexp.MustCompile(`"@message"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// trimLine strips JSON wrappers from Terraform's structured log stream so a
// line like {"@level":"error","@message":"...","@module":"terraform.ui"} is
// rendered as just the human-readable message. Falls back to the raw line
// when no @message field is present.
func trimLine(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "{") && strings.Contains(line, "@message") {
		if m := jsonMessageRe.FindStringSubmatch(line); len(m) == 2 {
			return unescapeJSONString(m[1])
		}
	}
	return line
}

func unescapeJSONString(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

var resourceAddrRe = regexp.MustCompile(`\b([a-z][a-z0-9_]*\.[A-Za-z_][A-Za-z0-9_-]*)\b`)

// extractResources pulls Terraform-style resource addresses (e.g.
// aws_vpc.main) out of the given text. Dedupes and caps at 5. Filters out
// obvious non-resource tokens: log module names (terraform.ui), URL fragments
// (registry.terraform — this is a greedy match over `registry.terraform.io`),
// and any match that is followed by `.` or `/` in the source, since that
// means we hit a hostname or path rather than a resource address.
func extractResources(text string, preferredLine string) []string {
	if text == "" && preferredLine == "" {
		return nil
	}
	// Known-good resource-name prefixes suggest the first segment is a Terraform
	// provider. A conservative denylist handles the log-wrapper noise without
	// false-rejecting legitimate resources.
	skipPrefixes := []string{
		"terraform.",
		"registry.",
		"hashicorp.",
		"github.",
		"golang.",
		"app.",
	}

	seen := map[string]bool{}
	out := make([]string, 0, 5)
	consider := func(body string) {
		matches := resourceAddrRe.FindAllStringSubmatchIndex(body, -1)
		for _, m := range matches {
			full := body[m[2]:m[3]]
			end := m[3]
			// Reject if the token continues into a URL/path/email (e.g.
			// "registry.terraform.io/...", "roshan.chandna@hashicorp.com").
			if end < len(body) {
				next := body[end]
				if next == '.' || next == '/' || next == '@' {
					continue
				}
			}
			// Terraform resource types are snake_case with at least one
			// underscore in the first segment (aws_vpc, random_id,
			// null_resource). This filters out filenames like "main.tf" and
			// names like "roshan.chandna" without maintaining a denylist.
			dot := strings.IndexByte(full, '.')
			if dot <= 0 || !strings.ContainsRune(full[:dot], '_') {
				continue
			}
			lower := strings.ToLower(full)
			skip := false
			for _, p := range skipPrefixes {
				if strings.HasPrefix(lower, p) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if seen[full] {
				continue
			}
			seen[full] = true
			out = append(out, full)
			if len(out) >= 5 {
				return
			}
		}
	}

	// Prefer the matched error line when it names a resource; fall back to the
	// broader tail so we catch the resource that was in-flight during the
	// error.
	if preferredLine != "" {
		consider(preferredLine)
	}
	if len(out) < 5 {
		consider(text)
	}
	return out
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

// failedPolicy is a decoded entry for a single failed policy in a policycheck
// list payload. enforcementLevel is empty when the field is absent.
type failedPolicy struct {
	name             string
	enforcementLevel string
}

// decodeFailedPolicies extracts only the failed policy entries from a `hcptf
// policycheck list -output=json` payload, preserving each policy's name and
// enforcement level. Returns nil when no failures are present so the diagnose
// flow can omit the field cleanly.
func decodeFailedPolicies(raw []byte) []failedPolicy {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	var out []failedPolicy
	for _, item := range arr {
		status := strings.ToLower(firstStringField(item, "status", "Status", "result", "Result"))
		switch status {
		case "failed", "fail", "hard_failed", "errored":
		default:
			continue
		}
		name := firstStringField(item, "name", "Name", "policy", "Policy", "policy_name", "PolicyName")
		if name == "" {
			name = firstStringField(item, "id", "ID")
		}
		if name == "" {
			continue
		}
		level := firstStringField(item,
			"enforcement_level", "EnforcementLevel", "enforcement-level",
			"enforce", "Enforce", "enforcement", "Enforcement",
		)
		out = append(out, failedPolicy{name: name, enforcementLevel: level})
	}
	return out
}

// policyNamePattern maps lowercased substrings found in a Sentinel/OPA policy
// name to a human-readable requirement. The first matching entry wins; order
// is from most-specific to least-specific.
type policyNamePattern struct {
	fragments   []string
	requirement string
}

var policyNamePatterns = []policyNamePattern{
	{[]string{"allowed-terraform-version", "terraform-version"}, "Upgrade your Terraform version"},
	{[]string{"restrict-ssh", "no-ssh"}, "Remove SSH (port 22) access from security groups"},
	{[]string{"required-tags", "enforce-tags"}, "Add required tags to all resources"},
	{[]string{"allowed-regions", "restrict-regions"}, "Move resources to an approved AWS region"},
	{[]string{"cost-limit", "budget"}, "Reduce estimated monthly cost below the policy threshold"},
}

// policyDefaultRequirement is the fallback prose returned by interpretPolicyName
// when no pattern in policyNamePatterns matches. It points the user at the
// policy source rather than guessing at semantics from an unfamiliar name.
const policyDefaultRequirement = "Review the policy source in the HCP Terraform UI to understand the requirements."

// interpretPolicyName runs the policyNamePatterns lookup and returns the
// matching requirement, or policyDefaultRequirement when nothing matches. Pure
// function — no I/O.
func interpretPolicyName(name string) string {
	lower := strings.ToLower(name)
	for _, p := range policyNamePatterns {
		for _, frag := range p.fragments {
			if strings.Contains(lower, frag) {
				return p.requirement
			}
		}
	}
	return policyDefaultRequirement
}

// interpretFailedPolicies decodes a policycheck list payload, runs each failed
// entry through the name lookup, and returns a JSON-ready slice. enforcement
// level is omitted when absent so the agent surfaces only what the API gave us.
func interpretFailedPolicies(raw []byte) []map[string]any {
	failed := decodeFailedPolicies(raw)
	if len(failed) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(failed))
	for _, fp := range failed {
		entry := map[string]any{
			"policy_name": fp.name,
			"requirement": interpretPolicyName(fp.name),
		}
		if fp.enforcementLevel != "" {
			entry["enforcement_level"] = fp.enforcementLevel
		}
		out = append(out, entry)
	}
	return out
}

// classifyRiskFactors groups resources by semantic category (IAM, security
// group, database, networking, load balancer) so the REPL and agent can
// surface the specific reasons a plan is risky. Returns the factors plus a
// "highest risk" resource list drawn from the most severe category that fired.
// riskReductionSuggestions builds a deduped, capped list of concrete actions a
// user can take to lower the assessed risk before applying. Suggestions are
// derived from the detected risk-factor names and the destruction count in
// blast_radius. Returns nil when no suggestions apply (caller omits the field).
func riskReductionSuggestions(level string, factors []map[string]any, plan planCounts) []string {
	const cap = 4
	out := []string{}
	add := func(s string) {
		if len(out) >= cap {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	for _, f := range factors {
		name, _ := f["factor"].(string)
		if strings.Contains(name, "IAM") {
			add("Review IAM permission boundaries and least-privilege policies before applying")
			add("Test IAM changes in a non-prod account first")
		}
		if strings.Contains(name, "Security group") {
			add("Review inbound/outbound rules against your security baseline before applying")
			add("Apply security group changes during a maintenance window")
		}
		if strings.Contains(name, "Networking") {
			add("Verify route tables and subnet associations in a staging environment first")
			add("Coordinate with on-call before applying networking changes")
		}
		if strings.Contains(name, "Database") {
			add("Ensure a database snapshot exists before applying")
			add("Test the change against a read replica first")
		}
	}
	if plan.destructions > 0 {
		add("Use terraform plan -target to apply non-destructive changes first")
		add("Take a state backup before applying: terraform state pull > backup.tfstate")
	}
	if len(out) == 0 && level == "Low" {
		return []string{"No specific risk reduction needed — proceed with normal review"}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func classifyRiskFactors(runResources []runResourceEntry, inventory []workspaceResource) ([]map[string]any, []string) {
	type group struct {
		factor    string
		severity  string
		resources []string
	}

	categories := []struct {
		factor    string
		severity  string
		matchType func(string) bool
		severityN int
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
// ~/.tfpilot/audit.log. Logging failures are reported to stderr and
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
	dir := filepath.Join(home, ".tfpilot")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.log"), nil
}

func expandHomeDir(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
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
	if name == "_hcp_tf_workspace_ownership" {
		return workspaceOwnershipCall(ctx, args, timeoutSec)
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
	if name == "_hcp_tf_run_diagnose" {
		return runDiagnoseCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_config_validate" {
		return configValidateCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_pr_create" {
		return prCreateCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspaces_list" {
		return workspacesListCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_version_audit" {
		return versionAuditCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_module_audit" {
		return moduleAuditCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_provider_audit" {
		return providerAuditCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_stacks_list" {
		return stacksListCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_stack_describe" {
		return stackDescribeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_stack_vs_workspace" {
		return stackVsWorkspaceCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspace_create" {
		return workspaceCreateCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspace_populate" {
		return workspacePopulateCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_upgrade_preview" {
		return upgradePreviewCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_drift_detect" {
		return driftDetectCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_org_timeline" {
		return orgTimelineCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_rollback" {
		return rollbackCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_incident_summary" {
		return incidentSummaryCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspace_dependencies" {
		return workspaceDependenciesCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_version_upgrade" {
		return versionUpgradeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_batch_upgrade" {
		return batchUpgradeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_compliance_report" {
		return complianceReportCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_compliance_summary" {
		return complianceSummaryCall(ctx, args, timeoutSec)
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

func workspaceCreateCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspace_create", Args: args}

	if err := require(args, "org", "name"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	org := args["org"]
	name := args["name"]

	cmdArgs := []string{"workspace", "create",
		"-org=" + org,
		"-name=" + name,
	}

	projectInput := strings.TrimSpace(args["project"])
	projectID := strings.TrimSpace(args["project_id"])
	resolvedProjectName := ""
	if projectID == "" && projectInput != "" {
		if strings.HasPrefix(projectInput, "prj-") {
			projectID = projectInput
		} else {
			id, pname, perr := resolveProjectID(ctx, org, projectInput, timeoutSec)
			if perr != nil {
				result.Err = perr
				result.Duration = time.Since(start)
				return result
			}
			projectID = id
			resolvedProjectName = pname
		}
	}
	if projectID != "" {
		cmdArgs = append(cmdArgs, "-project-id="+projectID)
	}

	if d := strings.TrimSpace(args["description"]); d != "" {
		cmdArgs = append(cmdArgs, "-description="+d)
	}
	if tv := strings.TrimSpace(args["terraform_version"]); tv != "" {
		cmdArgs = append(cmdArgs, "-terraform-version="+tv)
	}
	cmdArgs = append(cmdArgs, "-output=json")

	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, cmdArgs...)
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		parsed = map[string]any{}
	}

	out := map[string]any{
		"name": name,
		"org":  org,
	}
	if v, ok := parsed["ID"]; ok {
		out["workspace_id"] = v
	} else if v, ok := parsed["id"]; ok {
		out["workspace_id"] = v
	}
	if v, ok := parsed["Name"]; ok {
		out["name"] = v
	}
	if v, ok := parsed["Description"]; ok && v != nil && v != "" {
		out["description"] = v
	}
	if v, ok := parsed["TerraformVersion"]; ok && v != nil && v != "" {
		out["terraform_version"] = v
	}
	if projectID != "" {
		out["project_id"] = projectID
	}
	if resolvedProjectName != "" {
		out["project"] = resolvedProjectName
	} else if projectInput != "" && !strings.HasPrefix(projectInput, "prj-") {
		out["project"] = projectInput
	}
	out["url"] = fmt.Sprintf("https://app.terraform.io/app/%s/workspaces/%s", org, name)

	encoded, err := json.Marshal(out)
	if err != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "marshal workspace create result: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(encoded)
	result.Duration = time.Since(start)
	return result
}

func workspacePopulateCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspace_populate", Args: args}

	if err := require(args, "org", "workspace", "config"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]
	config := args["config"]
	message := strings.TrimSpace(args["message"])
	if message == "" {
		message = "tfpilot: initial resource provisioning"
	}

	dir, err := os.MkdirTemp("", "tfpilot-populate-*")
	if err != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "tempdir: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config), 0o644); err != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "write main.tf: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	initStatus := "skipped: terraform not on PATH"
	if _, err := exec.LookPath("terraform"); err == nil {
		ictx, icancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		initCmd := exec.CommandContext(ictx, "terraform", "init", "-backend=false", "-input=false", "-no-color")
		initCmd.Dir = dir
		if ierr := initCmd.Run(); ierr != nil {
			initStatus = "failed: " + ierr.Error()
		} else {
			initStatus = "ok"
		}
		icancel()
	}

	out := map[string]any{
		"workspace":      workspace,
		"org":            org,
		"terraform_init": initStatus,
	}

	cvRaw, cvErr := fetchHCPTFJSON(ctx, timeoutSec, "configversion", "create",
		"-org="+org, "-workspace="+workspace, "-output=json")
	if cvErr != nil {
		if strings.Contains(strings.ToLower(cvErr.Message), "unknown") || strings.Contains(strings.ToLower(cvErr.Message), "no such") {
			out["note"] = "Config version upload unavailable — workspace must be VCS-connected or config uploaded out of band."
		} else {
			result.Err = cvErr
			result.Duration = time.Since(start)
			return result
		}
	} else {
		var cvParsed map[string]any
		_ = json.Unmarshal(cvRaw, &cvParsed)
		cvID := ""
		for _, k := range []string{"ID", "id", "ConfigurationVersionID", "configuration_version_id"} {
			if v, ok := cvParsed[k].(string); ok && v != "" {
				cvID = v
				break
			}
		}
		uploadURL := ""
		for _, k := range []string{"UploadURL", "upload_url"} {
			if v, ok := cvParsed[k].(string); ok && v != "" {
				uploadURL = v
				break
			}
		}
		if cvID == "" {
			result.Err = &ToolError{ErrorCode: "execution_error", Message: "configversion create did not return an ID"}
			result.Duration = time.Since(start)
			return result
		}
		if uploadURL == "" {
			result.Err = &ToolError{ErrorCode: "execution_error", Message: "configversion create did not return an UploadURL (it is one-shot on create — the hcptf upload subcommand cannot refetch it)"}
			result.Duration = time.Since(start)
			return result
		}
		out["configuration_version_id"] = cvID

		tarball, terr := tarGzDir(dir)
		if terr != nil {
			result.Err = &ToolError{ErrorCode: "execution_error", Message: "tar.gz: " + terr.Error()}
			result.Duration = time.Since(start)
			return result
		}
		if uerr := putArchivist(ctx, uploadURL, tarball, timeoutSec); uerr != nil {
			result.Err = uerr
			result.Duration = time.Since(start)
			return result
		}
	}

	runRaw, rerr := fetchHCPTFJSON(ctx, timeoutSec, "run", "create",
		"-org="+org, "-workspace="+workspace, "-message="+message, "-output=json")
	if rerr != nil {
		result.Err = rerr
		result.Duration = time.Since(start)
		return result
	}
	var runParsed map[string]any
	_ = json.Unmarshal(runRaw, &runParsed)
	for _, k := range []string{"ID", "id", "RunID", "run_id"} {
		if v, ok := runParsed[k].(string); ok && v != "" {
			out["run_id"] = v
			break
		}
	}
	status := ""
	for _, k := range []string{"Status", "status"} {
		if v, ok := runParsed[k].(string); ok && v != "" {
			status = v
			break
		}
	}
	if status == "" {
		status = "pending"
	}
	out["status"] = status
	out["message"] = "Run triggered. Use _hcp_tf_runs_list_recent to check status."

	encoded, merr := json.Marshal(out)
	if merr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "marshal populate result: " + merr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(encoded)
	result.Duration = time.Since(start)
	return result
}

// versionUpgradeCall bumps a workspace's Terraform required_version to
// target_version by generating a minimal terraform{} HCL stub and routing
// through workspacePopulateCall. The caller is expected to chain into
// _hcp_tf_plan_analyze on the returned run_id and obtain explicit user
// approval before _hcp_tf_run_apply runs.
func versionUpgradeCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_version_upgrade", Args: args}

	if err := require(args, "org", "workspace", "target_version"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]
	targetVersion := args["target_version"]

	wsRaw, ferr := fetchWorkspaceRead(ctx, org, workspace, timeoutSec)
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}
	if mode := extractWorkspaceExecutionMode(wsRaw); mode == "local" {
		result.Err = &ToolError{
			ErrorCode: "unsupported_operation",
			Message:   fmt.Sprintf("Workspace %s is in local execution mode and cannot accept remote runs. Switch to remote execution in the HCP Terraform workspace settings and try again.", workspace),
			Retryable: false,
		}
		result.Duration = time.Since(start)
		return result
	}

	hcl := fmt.Sprintf("terraform {\n  required_version = \"~> %s\"\n}\n", targetVersion)

	populateArgs := map[string]string{
		"org":       org,
		"workspace": workspace,
		"config":    hcl,
		"message":   fmt.Sprintf("tfpilot: upgrade Terraform to %s", targetVersion),
	}
	populated := workspacePopulateCall(ctx, populateArgs, timeoutSec)
	if populated.Err != nil {
		result.Err = populated.Err
		result.Duration = time.Since(start)
		return result
	}

	var inner map[string]any
	if err := json.Unmarshal(populated.Output, &inner); err != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "decode populate output: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	runID, _ := inner["run_id"].(string)
	out := map[string]any{
		"org":            org,
		"workspace":      workspace,
		"target_version": targetVersion,
		"run_id":         inner["run_id"],
	}
	// HCP Terraform auto-finalizes plans with 0 changes into planned_and_finished,
	// which the apply gate cannot transition out of. checkRollbackNoop is generic
	// (polls run/show for the same terminal status) — reuse it so we flag no-op
	// upgrades before the agent walks the user into an apply trap.
	if runID != "" && checkRollbackNoop(ctx, runID, timeoutSec) {
		out["status"] = "planned_and_finished"
		out["is_noop"] = true
		out["message"] = fmt.Sprintf("Terraform version constraint ~> %s uploaded; the plan finished with no infrastructure changes. The version bump is complete — no apply needed.", targetVersion)
	} else {
		out["status"] = inner["status"]
		out["is_noop"] = false
		out["message"] = "Version bump config uploaded and plan triggered. Call _hcp_tf_plan_analyze with this run_id to assess risk before applying."
	}
	encoded, merr := json.Marshal(out)
	if merr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "marshal upgrade result: " + merr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(encoded)
	result.Duration = time.Since(start)
	return result
}

// severityRank maps an OSV severity string to a sort weight; higher = worse.
// Used by batchUpgradeCall to prioritize critical/high CVEs first.
func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium", "moderate":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// fetchLastRunDestructions returns the destruction count from a workspace's
// most recent finished plan, or 0 if no plan exists or the plan read fails.
// Used by batchUpgradeCall to flag workspaces whose previous run was
// destructive — those get auto-paused for human review even in "yes to all"
// mode.
func fetchLastRunDestructions(ctx context.Context, org, workspace string, timeoutSec int) int {
	wsRaw, err := fetchWorkspaceRead(ctx, org, workspace, timeoutSec)
	if err != nil {
		return 0
	}
	runID := extractCurrentRunID(wsRaw)
	if runID == "" {
		return 0
	}
	planRaw, perr := fetchHCPTFJSON(ctx, timeoutSec, "plan", "read", "-run-id="+runID, "-output=json")
	if perr != nil {
		return 0
	}
	return decodePlanCounts(planRaw).destructions
}

// batchUpgradeCall builds a prioritized upgrade queue for the listed
// workspaces. It does NOT execute the upgrades — the REPL drives the
// per-workspace approval loop. Marked mutating so the readonly-mode filter
// excludes it; the queue itself is read-only, but every queued workspace
// will be mutated as the REPL walks the loop.
func batchUpgradeCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_batch_upgrade", Args: args}

	if err := require(args, "org", "workspaces", "target_version"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	targetVersion := args["target_version"]
	mode := strings.TrimSpace(args["mode"])
	if mode == "" {
		mode = "interactive"
	}

	requested := []string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(args["workspaces"], ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		requested = append(requested, name)
	}
	if len(requested) == 0 {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: "workspaces must contain at least one workspace name"}
		result.Duration = time.Since(start)
		return result
	}

	auditResult := versionAuditCall(ctx, map[string]string{"org": org}, timeoutSec)
	if auditResult.Err != nil {
		result.Err = auditResult.Err
		result.Duration = time.Since(start)
		return result
	}
	var auditOut struct {
		VersionSummary []summaryEntry `json:"version_summary"`
	}
	if err := json.Unmarshal(auditResult.Output, &auditOut); err != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: "decode audit output: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	type wsAudit struct {
		version  string
		cves     []advisoryEntry
		severity string
	}
	auditByName := map[string]wsAudit{}
	for _, group := range auditOut.VersionSummary {
		highest := highestSeverity(group.KnownCVEs)
		for _, name := range group.Workspaces {
			auditByName[name] = wsAudit{version: group.TerraformVersion, cves: group.KnownCVEs, severity: highest}
		}
	}

	wsListResult := workspacesListCall(ctx, map[string]string{"org": org}, timeoutSec)
	resourceCount := map[string]int{}
	if wsListResult.Err == nil {
		var workspaces []map[string]any
		if err := json.Unmarshal(wsListResult.Output, &workspaces); err == nil {
			for _, ws := range workspaces {
				name := firstStringField(ws, "name", "Name")
				if name == "" {
					continue
				}
				resourceCount[name] = firstIntField(ws, "resource_count", "ResourceCount", "resource-count")
			}
		}
	}

	type queueEntry struct {
		Workspace       string `json:"workspace"`
		CurrentVersion  string `json:"current_version"`
		TargetVersion   string `json:"target_version"`
		CVECount        int    `json:"cve_count"`
		HighestSeverity string `json:"highest_severity"`
		CVEIDs          []string `json:"cve_ids,omitempty"`
		ResourceCount   int    `json:"resource_count"`
		LastDestroys    int    `json:"last_run_destructions"`
		RiskFlag        bool   `json:"risk_flag"`
		Priority        int    `json:"priority"`
	}

	entries := make([]queueEntry, len(requested))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, name := range requested {
		i := i
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			audit, hasAudit := auditByName[name]
			ids := make([]string, 0, len(audit.cves))
			for _, c := range audit.cves {
				ids = append(ids, c.ID)
			}
			rc := resourceCount[name]
			destroys := fetchLastRunDestructions(ctx, org, name, timeoutSec)
			version := audit.version
			if !hasAudit || version == "" {
				version = "unknown"
			}
			entries[i] = queueEntry{
				Workspace:       name,
				CurrentVersion:  version,
				TargetVersion:   targetVersion,
				CVECount:        len(audit.cves),
				HighestSeverity: audit.severity,
				CVEIDs:          ids,
				ResourceCount:   rc,
				LastDestroys:    destroys,
				RiskFlag:        rc > 50 || destroys > 0,
			}
		}()
	}
	wg.Wait()

	sort.SliceStable(entries, func(i, j int) bool {
		ri := severityRank(entries[i].HighestSeverity)
		rj := severityRank(entries[j].HighestSeverity)
		if ri != rj {
			return ri > rj
		}
		if entries[i].ResourceCount != entries[j].ResourceCount {
			return entries[i].ResourceCount > entries[j].ResourceCount
		}
		ip := strings.Contains(strings.ToLower(entries[i].Workspace), "prod")
		jp := strings.Contains(strings.ToLower(entries[j].Workspace), "prod")
		if ip != jp {
			return ip
		}
		return entries[i].Workspace < entries[j].Workspace
	})
	for i := range entries {
		entries[i].Priority = i + 1
	}

	out := map[string]any{
		"org":            org,
		"target_version": targetVersion,
		"mode":           mode,
		"total":          len(entries),
		"queue":          entries,
	}
	body, mErr := json.Marshal(out)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	return result
}

// batchResultIn is the shape the REPL hands to the compliance report tool —
// one entry per workspace the batch loop touched.
type batchResultIn struct {
	Workspace       string   `json:"workspace"`
	PreviousVersion string   `json:"previous_version"`
	NewVersion      string   `json:"new_version"`
	Status          string   `json:"status"`
	RiskScore       string   `json:"risk_score"`
	CVEsResolved    []string `json:"cves_resolved"`
	RunID           string   `json:"run_id"`
	ErrorCode       string   `json:"error_code"`
	DurationMs      int64    `json:"duration_ms"`
}

// complianceReportCall aggregates a batch upgrade's per-workspace results
// into a markdown report suitable for sharing with a CISO. Writes the
// markdown to the current directory as compliance-report-<timestamp>.md
// and returns the path. Pure local transformation — no HCP Terraform calls.
func complianceReportCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_compliance_report", Args: args}

	if err := require(args, "org", "results"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	var rows []batchResultIn
	if err := json.Unmarshal([]byte(args["results"]), &rows); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: "results must be a JSON array of batch result entries: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	upgraded := []batchResultIn{}
	skipped := []batchResultIn{}
	failed := []batchResultIn{}
	noop := []batchResultIn{}
	cveSet := map[string]bool{}
	for _, r := range rows {
		switch r.Status {
		case "applied":
			upgraded = append(upgraded, r)
			for _, id := range r.CVEsResolved {
				if id != "" {
					cveSet[id] = true
				}
			}
		case "noop":
			noop = append(noop, r)
		case "skipped":
			skipped = append(skipped, r)
		case "failed":
			failed = append(failed, r)
		}
	}
	cveIDs := make([]string, 0, len(cveSet))
	for id := range cveSet {
		cveIDs = append(cveIDs, id)
	}
	sort.Strings(cveIDs)

	now := time.Now().UTC()
	generatedAt := now.Format(time.RFC3339)
	humanDate := now.Format("January 2, 2006")
	targetVersion := strings.TrimSpace(args["target_version"])

	var b strings.Builder
	b.WriteString("# Infrastructure Security Compliance Report\n")
	fmt.Fprintf(&b, "**Organization:** %s  \n", args["org"])
	fmt.Fprintf(&b, "**Generated:** %s  \n", humanDate)
	if targetVersion != "" {
		fmt.Fprintf(&b, "**Target Terraform Version:** %s  \n", targetVersion)
	}
	b.WriteString("**Reviewed by:** tfpilot\n\n")

	b.WriteString("## Executive Summary\n")
	totalUpgraded := len(upgraded) + len(noop)
	switch {
	case totalUpgraded > 0 && len(failed) == 0 && len(skipped) == 0:
		fmt.Fprintf(&b, "%d of %d workspaces upgraded", totalUpgraded, len(rows))
		if targetVersion != "" {
			fmt.Fprintf(&b, " to Terraform %s", targetVersion)
		}
		b.WriteString(".")
	default:
		fmt.Fprintf(&b, "%d of %d workspaces upgraded", totalUpgraded, len(rows))
		if targetVersion != "" {
			fmt.Fprintf(&b, " to Terraform %s", targetVersion)
		}
		fmt.Fprintf(&b, ", %d skipped, %d failed.", len(skipped), len(failed))
	}
	if len(cveIDs) > 0 {
		fmt.Fprintf(&b, " %s resolved across all upgraded workspaces.", strings.Join(cveIDs, ", "))
	}
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("## Workspaces Upgraded (%d)\n", totalUpgraded))
	if totalUpgraded == 0 {
		b.WriteString("None.\n\n")
	} else {
		b.WriteString("| Workspace | Previous Version | New Version | Risk Score | CVEs Resolved |\n")
		b.WriteString("|-----------|-----------------|-------------|------------|---------------|\n")
		for _, r := range upgraded {
			cves := strings.Join(r.CVEsResolved, ", ")
			if cves == "" {
				cves = "—"
			}
			risk := r.RiskScore
			if risk == "" {
				risk = "Low"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Workspace, r.PreviousVersion, r.NewVersion, risk, cves)
		}
		for _, r := range noop {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Workspace, r.PreviousVersion, r.NewVersion, "noop (constraint only)", "—")
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("## Workspaces Skipped (%d)\n", len(skipped)))
	if len(skipped) == 0 {
		b.WriteString("None.\n\n")
	} else {
		b.WriteString("| Workspace | Reason |\n|-----------|--------|\n")
		for _, r := range skipped {
			reason := r.ErrorCode
			if reason == "" {
				reason = "skipped by user"
			}
			fmt.Fprintf(&b, "| %s | %s |\n", r.Workspace, reason)
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("## Workspaces Failed (%d)\n", len(failed)))
	if len(failed) == 0 {
		b.WriteString("None.\n\n")
	} else {
		b.WriteString("| Workspace | Error |\n|-----------|-------|\n")
		for _, r := range failed {
			err := r.ErrorCode
			if err == "" {
				err = "unknown"
			}
			fmt.Fprintf(&b, "| %s | %s |\n", r.Workspace, err)
		}
		b.WriteString("\n")
	}

	b.WriteString("## CVEs Resolved\n")
	if len(cveIDs) == 0 {
		b.WriteString("No CVEs were associated with the upgraded workspaces in this batch.\n")
	} else {
		for _, id := range cveIDs {
			fmt.Fprintf(&b, "- **%s** — resolved in upgraded workspaces.\n", id)
		}
	}

	report := b.String()

	var reportDir string
	if outputDir := strings.TrimSpace(args["output_dir"]); outputDir != "" {
		expanded, eerr := expandHomeDir(outputDir)
		if eerr != nil {
			result.Err = &ToolError{ErrorCode: "invalid_tool", Message: "output_dir: " + eerr.Error()}
			result.Duration = time.Since(start)
			return result
		}
		if mkErr := os.MkdirAll(expanded, 0o755); mkErr != nil {
			result.Err = &ToolError{ErrorCode: "execution_error", Message: "create report dir: " + mkErr.Error()}
			result.Duration = time.Since(start)
			return result
		}
		reportDir = expanded
	} else {
		cwd, _ := os.Getwd()
		if cwd == "" {
			cwd = "."
		}
		reportDir = cwd
	}
	fileName := fmt.Sprintf("compliance-report-%s.md", strings.ReplaceAll(now.Format("2006-01-02T15-04-05Z"), ":", "-"))
	reportPath := filepath.Join(reportDir, fileName)
	if werr := os.WriteFile(reportPath, []byte(report), 0o644); werr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "write report: " + werr.Error()}
		result.Duration = time.Since(start)
		return result
	}

	out := map[string]any{
		"org":            args["org"],
		"generated_at":   generatedAt,
		"target_version": targetVersion,
		"summary": map[string]any{
			"total_workspaces": len(rows),
			"upgraded":         len(upgraded),
			"noop":             len(noop),
			"skipped":          len(skipped),
			"failed":           len(failed),
			"cves_resolved":    len(cveIDs),
			"cve_ids_resolved": cveIDs,
		},
		"upgraded_workspaces": upgraded,
		"skipped_workspaces":  skipped,
		"failed_workspaces":   failed,
		"noop_workspaces":     noop,
		"report_markdown":     report,
		"report_path":         reportPath,
	}
	body, mErr := json.Marshal(out)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	_ = ctx
	_ = timeoutSec
	return result
}

// complianceSummaryCall produces an org-wide compliance posture snapshot:
// runs versionAuditCall to discover Terraform CVEs, optionally fans out
// providerAuditCall on the top-3 highest-resource at-risk workspaces, then
// derives a severity-weighted compliance_score, top_cves, remediation_priority,
// and a plain-English compliance_verdict. Read-only.
func complianceSummaryCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_compliance_summary", Args: args}

	if err := require(args, "org"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	includeProviders := strings.EqualFold(strings.TrimSpace(args["include_providers"]), "true")

	auditResult := versionAuditCall(ctx, map[string]string{"org": org}, timeoutSec)
	if auditResult.Err != nil {
		result.Err = auditResult.Err
		result.Duration = time.Since(start)
		return result
	}
	var auditOut struct {
		Org                    string         `json:"org"`
		WorkspaceCount         int            `json:"workspace_count"`
		VersionSummary         []summaryEntry `json:"version_summary"`
		LatestTerraformVersion string         `json:"latest_terraform_version"`
		WorkspacesAtRisk       int            `json:"workspaces_at_risk"`
		CVEDataUnavailable     bool           `json:"cve_data_unavailable"`
	}
	if err := json.Unmarshal(auditResult.Output, &auditOut); err != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: "decode audit output: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	wsListResult := workspacesListCall(ctx, map[string]string{"org": org}, timeoutSec)
	resourceCount := map[string]int{}
	if wsListResult.Err == nil {
		var workspaces []map[string]any
		if err := json.Unmarshal(wsListResult.Output, &workspaces); err == nil {
			for _, ws := range workspaces {
				name := firstStringField(ws, "name", "Name")
				if name == "" {
					continue
				}
				resourceCount[name] = firstIntField(ws, "resource_count", "ResourceCount", "resource-count")
			}
		}
	}

	type wsRecord struct {
		Workspace       string
		Version         string
		Status          string
		CVEs            []advisoryEntry
		HighestSeverity string
		ResourceCount   int
		IsProd          bool
	}

	records := make([]wsRecord, 0, auditOut.WorkspaceCount)
	for _, group := range auditOut.VersionSummary {
		highest := highestSeverity(group.KnownCVEs)
		for _, name := range group.Workspaces {
			rc := resourceCount[name]
			records = append(records, wsRecord{
				Workspace:       name,
				Version:         group.TerraformVersion,
				Status:          group.Status,
				CVEs:            group.KnownCVEs,
				HighestSeverity: highest,
				ResourceCount:   rc,
				IsProd:          strings.Contains(strings.ToLower(name), "prod"),
			})
		}
	}

	var compliancePtr *int
	var compliantWorkspaces, atRiskWorkspaces, criticalWorkspaces int
	if !auditOut.CVEDataUnavailable {
		var weightSum, weightCount int
		for _, r := range records {
			isCritical := false
			switch r.HighestSeverity {
			case "critical", "high":
				isCritical = true
			default:
				if r.IsProd && len(r.CVEs) > 0 {
					isCritical = true
				}
			}
			isAtRisk := r.Status != "current" || len(r.CVEs) > 0 || r.Version == "unknown"
			if !isAtRisk {
				compliantWorkspaces++
			} else {
				atRiskWorkspaces++
			}
			if isCritical {
				criticalWorkspaces++
			}

			if r.Version == "unknown" {
				continue
			}
			var weight int
			switch r.HighestSeverity {
			case "critical":
				weight = 0
			case "high":
				weight = 25
			case "medium", "moderate":
				weight = 50
			case "low":
				weight = 75
			default:
				weight = 100
			}
			weightSum += weight
			weightCount++
		}
		if weightCount > 0 {
			score := weightSum / weightCount
			compliancePtr = &score
		} else {
			zero := 0
			compliancePtr = &zero
		}
	} else {
		for _, r := range records {
			if r.Status == "current" && r.Version != "unknown" {
				compliantWorkspaces++
			} else {
				atRiskWorkspaces++
			}
			if r.IsProd && r.Status != "current" {
				criticalWorkspaces++
			}
		}
	}

	type cveAgg struct {
		ID                 string `json:"id"`
		Severity           string `json:"severity"`
		Summary            string `json:"summary,omitempty"`
		AffectedWorkspaces int    `json:"affected_workspaces"`
	}
	cveByID := map[string]*cveAgg{}
	for _, group := range auditOut.VersionSummary {
		for _, cve := range group.KnownCVEs {
			if cve.ID == "" {
				continue
			}
			agg, ok := cveByID[cve.ID]
			if !ok {
				agg = &cveAgg{ID: cve.ID, Severity: cve.Severity, Summary: cve.Summary}
				cveByID[cve.ID] = agg
			}
			agg.AffectedWorkspaces += group.WorkspaceCount
			if severityRank(cve.Severity) > severityRank(agg.Severity) {
				agg.Severity = cve.Severity
			}
		}
	}
	topCVEs := make([]cveAgg, 0, len(cveByID))
	for _, v := range cveByID {
		topCVEs = append(topCVEs, *v)
	}
	sort.SliceStable(topCVEs, func(i, j int) bool {
		ri, rj := severityRank(topCVEs[i].Severity), severityRank(topCVEs[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if topCVEs[i].AffectedWorkspaces != topCVEs[j].AffectedWorkspaces {
			return topCVEs[i].AffectedWorkspaces > topCVEs[j].AffectedWorkspaces
		}
		return topCVEs[i].ID < topCVEs[j].ID
	})
	if len(topCVEs) > 5 {
		topCVEs = topCVEs[:5]
	}

	type priorityEntry struct {
		Workspace       string `json:"workspace"`
		CurrentVersion  string `json:"current_version"`
		CVECount        int    `json:"cve_count"`
		HighestSeverity string `json:"highest_severity"`
		ResourceCount   int    `json:"resource_count"`
		IsProd          bool   `json:"is_prod"`
		Urgency         string `json:"urgency"`
		Reason          string `json:"reason"`
	}
	atRisk := make([]wsRecord, 0, len(records))
	for _, r := range records {
		if r.Status == "current" && len(r.CVEs) == 0 && r.Version != "unknown" {
			continue
		}
		atRisk = append(atRisk, r)
	}
	sort.SliceStable(atRisk, func(i, j int) bool {
		ri, rj := severityRank(atRisk[i].HighestSeverity), severityRank(atRisk[j].HighestSeverity)
		if ri != rj {
			return ri > rj
		}
		if atRisk[i].ResourceCount != atRisk[j].ResourceCount {
			return atRisk[i].ResourceCount > atRisk[j].ResourceCount
		}
		if atRisk[i].IsProd != atRisk[j].IsProd {
			return atRisk[i].IsProd
		}
		return atRisk[i].Workspace < atRisk[j].Workspace
	})
	priorityList := make([]priorityEntry, 0, 5)
	limit := 5
	if len(atRisk) < limit {
		limit = len(atRisk)
	}
	for _, r := range atRisk[:limit] {
		urgency := "medium"
		switch r.HighestSeverity {
		case "critical", "high":
			urgency = "critical"
		case "medium", "moderate":
			urgency = "high"
		case "low":
			urgency = "medium"
		}
		if r.IsProd && len(r.CVEs) > 0 && urgency != "critical" {
			urgency = "critical"
		}
		var reason string
		switch {
		case r.IsProd && len(r.CVEs) > 0:
			topID := ""
			if len(r.CVEs) > 0 {
				topID = r.CVEs[0].ID
				for _, c := range r.CVEs {
					if severityRank(c.Severity) > severityRank(r.HighestSeverity)-1 && c.ID != "" {
						topID = c.ID
						break
					}
				}
			}
			reason = fmt.Sprintf("Production workspace with %d resource%s affected by %s", r.ResourceCount, plural(r.ResourceCount), topID)
		case len(r.CVEs) > 0:
			reason = fmt.Sprintf("%d resource%s on Terraform %s with %d known CVE%s (%s)", r.ResourceCount, plural(r.ResourceCount), r.Version, len(r.CVEs), plural(len(r.CVEs)), r.HighestSeverity)
		case r.Version == "unknown":
			reason = "No Terraform version pinned — set one explicitly"
		default:
			reason = fmt.Sprintf("Outdated Terraform version %s with %d resource%s", r.Version, r.ResourceCount, plural(r.ResourceCount))
		}
		priorityList = append(priorityList, priorityEntry{
			Workspace:       r.Workspace,
			CurrentVersion:  r.Version,
			CVECount:        len(r.CVEs),
			HighestSeverity: r.HighestSeverity,
			ResourceCount:   r.ResourceCount,
			IsProd:          r.IsProd,
			Urgency:         urgency,
			Reason:          reason,
		})
	}

	type providerAuditOut struct {
		Workspace         string          `json:"workspace"`
		ProvidersWithCVEs []providerBrief `json:"providers_with_cves"`
		ErrorCode         string          `json:"error_code,omitempty"`
	}
	providerAudits := []providerAuditOut{}
	providerDataPartial := false
	if includeProviders && len(atRisk) > 0 {
		topN := 3
		if len(atRisk) < topN {
			topN = len(atRisk)
		}
		audits := make([]providerAuditOut, topN)
		var wg sync.WaitGroup
		sem := make(chan struct{}, topN)
		for i := 0; i < topN; i++ {
			i := i
			ws := atRisk[i].Workspace
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				pr := providerAuditCall(ctx, map[string]string{"org": org, "workspace": ws}, timeoutSec)
				out := providerAuditOut{Workspace: ws, ProvidersWithCVEs: []providerBrief{}}
				if pr.Err != nil {
					out.ErrorCode = pr.Err.ErrorCode
					audits[i] = out
					return
				}
				var pa struct {
					Providers []struct {
						Name              string          `json:"name"`
						RegistryPath      string          `json:"registry_path"`
						PinnedVersion     string          `json:"pinned_version"`
						LatestVersion     string          `json:"latest_version"`
						CurrentlyAffected []advisoryEntry `json:"currently_affected"`
						UpgradingFixes    []advisoryEntry `json:"upgrading_fixes"`
					} `json:"providers"`
					CVEDataUnavailable bool `json:"cve_data_unavailable"`
				}
				if uerr := json.Unmarshal(pr.Output, &pa); uerr != nil {
					out.ErrorCode = "parse_error"
					audits[i] = out
					return
				}
				if pa.CVEDataUnavailable {
					out.ErrorCode = "cve_data_unavailable"
				}
				for _, p := range pa.Providers {
					if len(p.CurrentlyAffected) == 0 && len(p.UpgradingFixes) == 0 {
						continue
					}
					out.ProvidersWithCVEs = append(out.ProvidersWithCVEs, providerBrief{
						Name:              p.Name,
						RegistryPath:      p.RegistryPath,
						PinnedVersion:     p.PinnedVersion,
						LatestVersion:     p.LatestVersion,
						CurrentlyAffected: len(p.CurrentlyAffected),
						UpgradingFixes:    len(p.UpgradingFixes),
					})
				}
				audits[i] = out
			}()
		}
		wg.Wait()
		for _, a := range audits {
			if a.ErrorCode != "" {
				providerDataPartial = true
			}
			providerAudits = append(providerAudits, a)
		}
	}

	totalWorkspaces := len(records)
	if totalWorkspaces == 0 {
		totalWorkspaces = auditOut.WorkspaceCount
	}

	verdict := ""
	readyForReview := false
	switch {
	case auditOut.CVEDataUnavailable || compliancePtr == nil:
		verdict = "Compliance status indeterminate — CVE data unavailable. Retry shortly or check OSV.dev availability."
	case *compliancePtr >= 90:
		verdict = "✓ No action needed — infrastructure is in good shape for review."
		readyForReview = true
	case *compliancePtr >= 70:
		if criticalWorkspaces > 0 {
			verdict = fmt.Sprintf("⚠ Action required before review — %d critical workspace%s need attention.", criticalWorkspaces, plural(criticalWorkspaces))
		} else {
			verdict = fmt.Sprintf("⚠ Action required before review — %d workspace%s have known vulnerabilities.", atRiskWorkspaces, plural(atRiskWorkspaces))
		}
	default:
		verdict = fmt.Sprintf("✗ Significant vulnerabilities detected — remediation required before the review. %d of %d workspaces have known vulnerabilities.", atRiskWorkspaces, totalWorkspaces)
	}

	out := map[string]any{
		"org":                   org,
		"generated_at":          time.Now().UTC().Format(time.RFC3339),
		"compliance_score":      compliancePtr,
		"compliance_verdict":    verdict,
		"ready_for_review":      readyForReview,
		"total_workspaces":      totalWorkspaces,
		"compliant_workspaces":  compliantWorkspaces,
		"at_risk_workspaces":    atRiskWorkspaces,
		"critical_workspaces":   criticalWorkspaces,
		"top_cves":              topCVEs,
		"remediation_priority":  priorityList,
		"cve_data_unavailable":  auditOut.CVEDataUnavailable,
		"provider_data_partial": providerDataPartial,
		"provider_audits":       providerAudits,
		"include_providers":     includeProviders,
	}
	body, mErr := json.Marshal(out)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	return result
}

// providerBrief is the compact provider-with-CVEs entry surfaced by
// complianceSummaryCall when include_providers is enabled. Drops noisy fields
// like AllCVEs to keep the compliance payload small.
type providerBrief struct {
	Name              string `json:"name"`
	RegistryPath      string `json:"registry_path"`
	PinnedVersion     string `json:"pinned_version"`
	LatestVersion     string `json:"latest_version"`
	CurrentlyAffected int    `json:"currently_affected"`
	UpgradingFixes    int    `json:"upgrading_fixes"`
}

// tarGzDir walks dir and returns a gzipped tar archive of every regular file
// inside it, with paths relative to dir. Used to hand configuration bundles to
// the HCP Terraform archivist service, which ingests tar.gz payloads.
func tarGzDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		hdr := &tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode().Perm()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if _, werr := io.Copy(tw, f); werr != nil {
			return werr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// putArchivist PUTs an octet-stream payload to the one-shot upload URL
// returned by `hcptf configversion create`. Treated as a raw HTTP call rather
// than an hcptf subcommand because the URL expires almost immediately after
// create, so it cannot be re-fetched by a second hcptf invocation.
func putArchivist(ctx context.Context, url string, body []byte, timeoutSec int) *ToolError {
	rctx, rcancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer rcancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return &ToolError{ErrorCode: "execution_error", Message: "build upload request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolError{ErrorCode: "execution_error", Message: "upload: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &ToolError{ErrorCode: "execution_error", Message: fmt.Sprintf("upload returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))}
	}
	return nil
}

func resolveProjectID(ctx context.Context, org, projectName string, timeoutSec int) (string, string, *ToolError) {
	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "project", "list", "-org="+org, "-output=json")
	if ferr != nil {
		return "", "", ferr
	}
	var projects []map[string]any
	if err := json.Unmarshal(raw, &projects); err != nil {
		return "", "", &ToolError{ErrorCode: "execution_error", Message: "parse project list: " + err.Error()}
	}
	needle := strings.ToLower(projectName)
	for _, p := range projects {
		if n, ok := p["Name"].(string); ok && strings.ToLower(n) == needle {
			if id, ok := p["ID"].(string); ok {
				return id, n, nil
			}
		}
	}
	return "", "", &ToolError{ErrorCode: "not_found", Message: fmt.Sprintf("project %q not found in org %q", projectName, org), Retryable: false}
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
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	// hcptf occasionally prints a plain-text "No resources found" line for empty
	// workspaces instead of an empty JSON array. Treat any non-JSON payload as
	// an empty resource list rather than surfacing a parse error.
	if first := trimmed[0]; first != '[' && first != '{' {
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

// workspaceOwnershipCall returns workspace ownership and metadata. It merges
// `hcptf workspace read` (timestamps, VCS repo, description, current run ID),
// `hcptf team access list` (team-level permissions), and the HCP Terraform
// HTTP API GET /runs/<id>?include=created_by (last modifier user attribution).
// inferred_owner is the admin team's name when one exists, otherwise the last
// modifier's username, otherwise "unknown". Read-only.
func workspaceOwnershipCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspace_ownership", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	raw, ferr := fetchWorkspaceRead(ctx, org, workspace, timeoutSec)
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}

	createdAt, updatedAt := extractWorkspaceTimestamps(raw)
	vcsRepo := extractVCSRepoIdentifier(raw)
	wsID := extractWorkspaceID(raw)
	description := extractWorkspaceDescription(raw)
	currentRunID := extractCurrentRunID(raw)

	teamAccess, teamNote := fetchTeamAccess(ctx, wsID, timeoutSec)
	lastMod, lastModNote := fetchLastModifiedBy(ctx, currentRunID, timeoutSec)

	payload := map[string]any{
		"workspace":          workspace,
		"org":                org,
		"created_at":         createdAt,
		"created_at_human":   humanizeISO(createdAt),
		"last_updated":       updatedAt,
		"last_updated_human": humanizeISO(updatedAt),
		"team_access":        teamAccess,
		"team_access_note":   teamNote,
		"inferred_owner":     computeInferredOwner(teamAccess, lastMod),
	}
	if vcsRepo != "" {
		payload["vcs_repo"] = vcsRepo
	} else {
		payload["vcs_repo"] = nil
	}
	if description != "" {
		payload["description"] = description
	} else {
		payload["description"] = nil
	}
	if lastMod != nil {
		payload["last_modified_by"] = lastMod
	} else {
		payload["last_modified_by"] = nil
	}
	if lastModNote != "" {
		payload["last_modified_by_note"] = lastModNote
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

// extractWorkspaceID pulls the workspace ID (ws-XXX) out of a `hcptf workspace
// read -output=json` payload. Probes both Go-mapped (`ID`) and JSON:API (`id`,
// nested `attributes`) shapes; returns "" on miss.
func extractWorkspaceID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if id := firstStringField(m, "ID", "id", "workspace_id", "WorkspaceID"); id != "" {
		return id
	}
	if attrs, ok := m["attributes"].(map[string]any); ok {
		if id := firstStringField(attrs, "id", "ID"); id != "" {
			return id
		}
	}
	return ""
}

// extractWorkspaceExecutionMode pulls the execution mode ("remote", "local",
// "agent") out of a `hcptf workspace read -output=json` payload. Probes both
// Go-mapped and JSON:API shapes; returns "" on miss.
func extractWorkspaceExecutionMode(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v := firstStringField(m, "ExecutionMode", "execution_mode", "execution-mode"); v != "" {
		return v
	}
	if attrs, ok := m["attributes"].(map[string]any); ok {
		if v := firstStringField(attrs, "execution-mode", "execution_mode", "ExecutionMode"); v != "" {
			return v
		}
	}
	return ""
}

// extractWorkspaceDescription pulls the description string out of a workspace
// read payload. Returns "" when absent or empty.
func extractWorkspaceDescription(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if d := firstStringField(m, "Description", "description"); d != "" {
		return d
	}
	if attrs, ok := m["attributes"].(map[string]any); ok {
		if d := firstStringField(attrs, "description", "Description"); d != "" {
			return d
		}
	}
	return ""
}

// extractCurrentRunID pulls the workspace's most recent run ID from the
// workspace read payload. Returns "" when no recent run exists.
func extractCurrentRunID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if id := firstStringField(m, "CurrentRunID", "current-run-id", "current_run_id"); id != "" {
		return id
	}
	if attrs, ok := m["attributes"].(map[string]any); ok {
		if id := firstStringField(attrs, "current-run-id", "current_run_id"); id != "" {
			return id
		}
	}
	return ""
}

// userInfo carries the username + email for the most recent run's creator.
// Email is best-effort: HCP Terraform's runs?include=created_by often omits it
// when the caller lacks the right scope.
type userInfo struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
}

// fetchTeamAccess shells out to `hcptf team access list -workspace-id=<id>`
// and parses the JSON response. The CLI prints "No team access found" (exit
// 0) when the workspace has no team-level permissions defined — that is
// returned as (empty, note) rather than as an error so the caller can still
// emit a useful payload.
func fetchTeamAccess(ctx context.Context, wsID string, timeoutSec int) ([]map[string]string, string) {
	if wsID == "" {
		return []map[string]string{}, "Workspace ID unavailable; team access could not be queried."
	}
	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "team", "access", "list", "-workspace-id="+wsID, "-output=json")
	if ferr != nil {
		return []map[string]string{}, "Team access could not be retrieved: " + ferr.Message
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || strings.Contains(trimmed, "No team access found") {
		return []map[string]string{}, "No team-level access defined; check workspace permissions in the HCP Terraform UI."
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return []map[string]string{}, "Team access response was not parseable JSON."
	}
	out := make([]map[string]string, 0, len(arr))
	for _, item := range arr {
		teamName := firstStringField(item, "TeamName", "team_name", "team-name", "team", "Team")
		if teamName == "" {
			if t, ok := item["Team"].(map[string]any); ok {
				teamName = firstStringField(t, "Name", "name")
			}
		}
		access := firstStringField(item, "Access", "access", "permission", "Permission")
		if teamName == "" && access == "" {
			continue
		}
		out = append(out, map[string]string{"team": teamName, "access": access})
	}
	if len(out) == 0 {
		return []map[string]string{}, "Team access list returned no recognizable entries."
	}
	return out, "Team access sourced from HCP Terraform workspace permissions."
}

// fetchLastModifiedBy resolves the user who created the workspace's most
// recent run via GET /api/v2/runs/<id>?include=created_by. Returns nil with a
// note when no run exists, the API token is missing, the API call fails, or
// the run was triggered without user attribution (e.g. via an org-level API
// token).
func fetchLastModifiedBy(ctx context.Context, runID string, timeoutSec int) (*userInfo, string) {
	if runID == "" {
		return nil, "No recent run found for this workspace."
	}
	token := readTFCToken()
	if token == "" {
		return nil, "Last modifier could not be resolved: no HCP Terraform API token available locally."
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		"https://app.terraform.io/api/v2/runs/"+runID+"?include=created_by", nil)
	if err != nil {
		return nil, "Last modifier lookup failed: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "Last modifier lookup failed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Sprintf("Last modifier lookup failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "Last modifier lookup failed: " + err.Error()
	}
	var doc struct {
		Data struct {
			Relationships struct {
				CreatedBy struct {
					Data *struct {
						ID   string `json:"id"`
						Type string `json:"type"`
					} `json:"data"`
				} `json:"created-by"`
			} `json:"relationships"`
		} `json:"data"`
		Included []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				Username string `json:"username"`
				Email    string `json:"email"`
			} `json:"attributes"`
		} `json:"included"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "Last modifier response was not parseable JSON."
	}
	if doc.Data.Relationships.CreatedBy.Data == nil {
		return nil, "Last run was triggered without user attribution (likely an org-level API token)."
	}
	wantID := doc.Data.Relationships.CreatedBy.Data.ID
	for _, inc := range doc.Included {
		if inc.Type == "users" && inc.ID == wantID {
			return &userInfo{Username: inc.Attributes.Username, Email: inc.Attributes.Email}, ""
		}
	}
	return nil, "Last modifier user record could not be resolved from the run API response."
}

// computeInferredOwner picks the most defensible owner signal: an admin team
// when one exists (groups outrank individuals for ownership purposes), else
// the most recent modifier's username, else "unknown".
func computeInferredOwner(teamAccess []map[string]string, lastMod *userInfo) string {
	for _, t := range teamAccess {
		if strings.EqualFold(t["access"], "admin") && t["team"] != "" {
			return t["team"]
		}
	}
	if lastMod != nil && lastMod.Username != "" {
		return lastMod.Username
	}
	return "unknown"
}

// humanizeISO parses an ISO-8601 timestamp and returns a relative string
// ("3 days ago"). Returns "" for empty input or parse failure so callers can
// drop the field cleanly.
func humanizeISO(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339Nano, iso)
		if err2 != nil {
			return ""
		}
		t = t2
	}
	return humanRelative(t)
}

// extractWorkspaceTimestamps pulls created/updated ISO strings out of a
// `hcptf workspace read -output=json` payload. The CLI's JSON shape is not
// fully documented; probe both kebab-case (HCP API style) and Go-mapped names.
// Returns empty strings when neither shape matches.
func extractWorkspaceTimestamps(raw []byte) (createdAt, updatedAt string) {
	if len(raw) == 0 {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	pickString := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		// Probe a nested attributes block too (JSON:API style).
		if attrs, ok := m["attributes"].(map[string]any); ok {
			for _, k := range keys {
				if v, ok := attrs[k].(string); ok && v != "" {
					return v
				}
			}
		}
		return ""
	}
	createdAt = pickString("CreatedAt", "created-at", "created_at", "createdAt")
	updatedAt = pickString("UpdatedAt", "updated-at", "updated_at", "updatedAt")
	return createdAt, updatedAt
}

// extractVCSRepoIdentifier pulls the VCS repo identifier (e.g. "acme/infra")
// out of a `workspace read` payload. Returns "" when the workspace is not
// connected to a VCS or when the field shape is unrecognized.
func extractVCSRepoIdentifier(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	probe := func(obj map[string]any) string {
		if obj == nil {
			return ""
		}
		for _, k := range []string{"vcs-repo", "vcs_repo", "VcsRepo", "VCSRepo"} {
			node, ok := obj[k].(map[string]any)
			if !ok {
				continue
			}
			for _, idKey := range []string{"identifier", "Identifier", "display-identifier", "DisplayIdentifier"} {
				if v, ok := node[idKey].(string); ok && v != "" {
					return v
				}
			}
		}
		return ""
	}
	if id := probe(m); id != "" {
		return id
	}
	if attrs, ok := m["attributes"].(map[string]any); ok {
		if id := probe(attrs); id != "" {
			return id
		}
	}
	return ""
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

// workspacesListCall shells out to `hcptf workspace list` and enriches each
// workspace with resource_count and current_run_status by fanning out parallel
// `workspace read` calls. The base `workspace list` payload only includes
// name/id/locked/terraform-version — without the fan-out the /workspaces
// render would always show "0 resources" and "no runs". Read-only; visible
// in every mode.
func workspacesListCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_workspaces_list", Args: args}

	if err := require(args, "org"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]

	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "list", "-org="+org, "-output=json")
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}

	var workspaces []map[string]any
	if err := json.Unmarshal(raw, &workspaces); err != nil {
		result.Output = json.RawMessage(raw)
		result.Duration = time.Since(start)
		return result
	}

	var wg sync.WaitGroup
	for i := range workspaces {
		ws := workspaces[i]
		name := firstStringField(ws, "name", "Name")
		if name == "" {
			ws["resource_count"] = 0
			ws["current_run_status"] = ""
			continue
		}
		wg.Add(1)
		go func(w map[string]any, n string) {
			defer wg.Done()
			readRaw, rerr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "read", "-org="+org, "-name="+n, "-output=json")
			if rerr != nil {
				w["resource_count"] = 0
				w["current_run_status"] = ""
				return
			}
			var detail map[string]any
			if err := json.Unmarshal(readRaw, &detail); err != nil {
				w["resource_count"] = 0
				w["current_run_status"] = ""
				return
			}
			w["resource_count"] = firstIntField(detail, "ResourceCount", "resource-count", "resource_count")
			w["current_run_status"] = firstStringField(detail, "CurrentRunStatus", "current-run-status", "current_run_status")
		}(ws, name)
	}
	wg.Wait()

	out, mErr := json.Marshal(workspaces)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// latestTerraformVersion is the upstream baseline used by versionAuditCall to
// compute "versions_behind". Bump when a new minor releases on
// https://github.com/hashicorp/terraform/releases/latest.
const latestTerraformVersion = "1.14.9"

type advisoryEntry struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
	FixedIn  string `json:"fixed_in,omitempty"`
}

// versionAuditCall groups every workspace in the org by its Terraform version,
// queries OSV.dev once per unique version for known CVEs, scores upgrade
// complexity, and returns a structured org-wide audit. Read-only; the OSV
// fetch degrades gracefully (cve_data_unavailable=true) on any HTTP failure.
func versionAuditCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_version_audit", Args: args}

	if err := require(args, "org"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	wsResult := workspacesListCall(ctx, map[string]string{"org": args["org"]}, timeoutSec)
	if wsResult.Err != nil {
		result.Err = wsResult.Err
		result.Duration = time.Since(start)
		return result
	}

	var workspaces []map[string]any
	if err := json.Unmarshal(wsResult.Output, &workspaces); err != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: fmt.Sprintf("could not parse workspaces list: %v", err)}
		result.Duration = time.Since(start)
		return result
	}

	type wsInfo struct {
		name      string
		resources int
	}
	versionToWS := map[string][]wsInfo{}
	for _, ws := range workspaces {
		name := firstStringField(ws, "name", "Name")
		if name == "" {
			continue
		}
		ver := firstStringField(ws, "terraform_version", "TerraformVersion", "terraform-version", "Terraform Version")
		ver = normalizeTerraformVersion(ver)
		if ver == "" {
			ver = "unknown"
		}
		versionToWS[ver] = append(versionToWS[ver], wsInfo{
			name:      name,
			resources: firstIntField(ws, "resource_count", "ResourceCount", "resource-count"),
		})
	}

	advisoryCache := map[string][]advisoryEntry{}
	cveDataUnavailable := false
	type osvResult struct {
		version string
		entries []advisoryEntry
		failed  bool
	}
	var mu sync.Mutex
	var owg sync.WaitGroup
	results := make([]osvResult, 0, len(versionToWS))
	sem := make(chan struct{}, 8)
	for ver := range versionToWS {
		if ver == "unknown" {
			advisoryCache[ver] = nil
			continue
		}
		owg.Add(1)
		go func(v string) {
			defer owg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries, fetchErr := fetchOSVAdvisories(ctx, v, timeoutSec)
			mu.Lock()
			results = append(results, osvResult{version: v, entries: entries, failed: fetchErr})
			mu.Unlock()
		}(ver)
	}
	owg.Wait()
	for _, r := range results {
		advisoryCache[r.version] = r.entries
		if r.failed {
			cveDataUnavailable = true
		}
	}

	summaries := make([]summaryEntry, 0, len(versionToWS))
	atRisk := 0
	for ver, list := range versionToWS {
		names := make([]string, 0, len(list))
		maxResources := 0
		for _, w := range list {
			names = append(names, w.name)
			if w.resources > maxResources {
				maxResources = w.resources
			}
		}
		sort.Strings(names)

		behind := versionsBehind(ver, latestTerraformVersion)
		majorJump := majorComponent(ver) >= 0 && majorComponent(ver) < majorComponent(latestTerraformVersion)
		cves := advisoryCache[ver]
		highestSev := highestSeverity(cves)

		complexity := upgradeComplexity(maxResources, behind, majorJump)

		status := "current"
		if behind > 5 || majorJump || highestSev == "high" || highestSev == "critical" {
			status = "critical"
		} else if behind >= 2 || (behind > 0 && len(cves) > 0) || highestSev == "low" || highestSev == "medium" {
			status = "outdated"
		}
		if ver == "unknown" {
			status = "outdated"
		}
		if status != "current" {
			atRisk += len(list)
		}

		notes := upgradeNotes(ver, latestTerraformVersion, behind, majorJump, len(cves))

		summaries = append(summaries, summaryEntry{
			TerraformVersion:  ver,
			WorkspaceCount:    len(list),
			Workspaces:        names,
			Status:            status,
			VersionsBehind:    behind,
			KnownCVEs:         cves,
			CVECount:          len(cves),
			UpgradeComplexity: complexity,
			UpgradeNotes:      notes,
		})
	}

	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].VersionsBehind != summaries[j].VersionsBehind {
			return summaries[i].VersionsBehind > summaries[j].VersionsBehind
		}
		return summaries[i].TerraformVersion < summaries[j].TerraformVersion
	})

	recommendation := buildRecommendation(summaries, cveDataUnavailable)

	out := map[string]any{
		"org":                      args["org"],
		"workspace_count":          len(workspaces),
		"version_summary":          summaries,
		"latest_terraform_version": latestTerraformVersion,
		"workspaces_at_risk":       atRisk,
		"recommendation":           recommendation,
		"cve_data_unavailable":     cveDataUnavailable,
	}

	body, mErr := json.Marshal(out)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	return result
}

// moduleAuditNote is appended to every _hcp_tf_module_audit response so the
// caller is reminded that pinned versions cannot be inferred from resource
// addresses alone.
const moduleAuditNote = "Module versions are inferred from resource addresses. Pinned versions are not available without access to the Terraform configuration files. Compare the latest versions above against your module source blocks."

// moduleRegistryLookup maps the local module instance name observed in a
// workspace's resource addresses to the canonical Terraform Registry path.
// The set is intentionally small: it covers the well-known terraform-aws-modules
// names we expect to see in the test orgs. Names absent from this map fall into
// unknown_modules and are surfaced for the user to inspect by hand.
var moduleRegistryLookup = map[string]string{
	"vpc":                  "terraform-aws-modules/vpc/aws",
	"app_security_group":   "terraform-aws-modules/security-group/aws",
	"lb_security_group":    "terraform-aws-modules/security-group/aws",
	"elb_http":             "terraform-aws-modules/elb/aws",
	"elb":                  "terraform-aws-modules/elb/aws",
	"alb":                  "terraform-aws-modules/alb/aws",
	"eks":                  "terraform-aws-modules/eks/aws",
	"rds":                  "terraform-aws-modules/rds/aws",
	"s3_bucket":            "terraform-aws-modules/s3-bucket/aws",
	"iam_role":             "terraform-aws-modules/iam/aws",
	"iam_policy":           "terraform-aws-modules/iam/aws",
	"autoscaling":          "terraform-aws-modules/autoscaling/aws",
	"lambda_function":      "terraform-aws-modules/lambda/aws",
	"route53_records":      "terraform-aws-modules/route53/aws",
	"acm":                  "terraform-aws-modules/acm/aws",
	"kms":                  "terraform-aws-modules/kms/aws",
	"cloudwatch_log_group": "terraform-aws-modules/cloudwatch/aws",
	"transit_gateway":      "terraform-aws-modules/transit-gateway/aws",
	"managed_node_group":   "terraform-aws-modules/eks/aws",
	"fargate_profile":      "terraform-aws-modules/eks/aws",
}

// extractModuleInstanceNames returns the distinct top-level module instance
// names observed across the resource list. hcptf surfaces the Module field as a
// dot-separated path (e.g. "vpc", "lb_security_group.sg") with "root" used for
// resources that live outside any module — we take the leading segment, drop
// "root", and dedupe.
func extractModuleInstanceNames(items []workspaceResource) []string {
	skip := map[string]bool{"": true, "root": true, "data": true, "local": true, "var": true, "output": true}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		path := strings.TrimSpace(it.Module)
		if path == "" {
			// Fall back to the Address — the first dot-separated token is the
			// module instance name when the resource lives inside a module.
			if it.Address == "" {
				continue
			}
			path = it.Address
		}
		first := path
		if idx := strings.Index(path, "."); idx >= 0 {
			first = path[:idx]
		}
		// Strip count/for_each suffixes ("ec2_instances[0]", `cluster["us-east-1"]`)
		// so module entries dedupe across instances of the same source.
		if idx := strings.Index(first, "["); idx > 0 {
			first = first[:idx]
		}
		if skip[first] {
			continue
		}
		if _, ok := seen[first]; ok {
			continue
		}
		seen[first] = struct{}{}
		out = append(out, first)
	}
	sort.Strings(out)
	return out
}

// registryModule mirrors the JSON shape of `hcptf publicregistry module`.
type registryModule struct {
	Name        string `json:"Name"`
	Version     string `json:"Version"`
	Description string `json:"Description"`
	DocsURL     string `json:"DocsURL"`
	Source      string `json:"Source"`
}

// fetchRegistryModule shells out to `hcptf publicregistry module` for a single
// registry path and returns the parsed metadata or a sentinel "unavailable"
// flag when the registry call fails (HTML guard, exec error, or parse error).
// Failures never abort the whole audit — they degrade per-module.
func fetchRegistryModule(ctx context.Context, registryPath string, timeoutSec int) (*registryModule, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", "publicregistry", "module",
		"-name="+registryPath,
		"-output=json",
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		return nil, true
	}
	if looksLikeHTML(string(out)) {
		return nil, true
	}
	var mod registryModule
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, true
	}
	if mod.Version == "" {
		return nil, true
	}
	return &mod, false
}

// moduleAuditCall infers Terraform Registry modules used by a workspace from
// its resource addresses and surfaces the latest available version for each
// known module via `hcptf publicregistry module`. Pinned versions are not
// available without the underlying .tf files, so every entry is labeled
// "check_recommended" and the response carries a note explaining the limit.
// Read-only.
func moduleAuditCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_module_audit", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	raw, fetchErr := fetchWorkspaceResources(ctx, org, workspace, timeoutSec)
	if fetchErr != nil {
		result.Err = fetchErr
		result.Duration = time.Since(start)
		return result
	}
	items, perr := unmarshalResources(raw)
	if perr != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: perr.Error()}
		result.Duration = time.Since(start)
		return result
	}

	names := extractModuleInstanceNames(items)

	var unknown []string
	pathToInstances := map[string][]string{}
	for _, n := range names {
		if path, ok := moduleRegistryLookup[n]; ok {
			pathToInstances[path] = append(pathToInstances[path], n)
		} else {
			unknown = append(unknown, n)
		}
	}

	uniquePaths := make([]string, 0, len(pathToInstances))
	for p := range pathToInstances {
		uniquePaths = append(uniquePaths, p)
	}
	sort.Strings(uniquePaths)

	type fetched struct {
		path string
		mod  *registryModule
		bad  bool
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	results := make([]fetched, 0, len(uniquePaths))
	for _, p := range uniquePaths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mod, bad := fetchRegistryModule(ctx, path, timeoutSec)
			mu.Lock()
			results = append(results, fetched{path: path, mod: mod, bad: bad})
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	pathToFetched := make(map[string]fetched, len(results))
	for _, r := range results {
		pathToFetched[r.path] = r
	}

	type moduleEntry struct {
		InferredNames []string `json:"inferred_names"`
		RegistryPath  string   `json:"registry_path"`
		LatestVersion string   `json:"latest_version"`
		Description   string   `json:"description"`
		DocsURL       string   `json:"docs_url"`
		PinnedVersion string   `json:"pinned_version"`
		Status        string   `json:"status"`
	}
	entries := make([]moduleEntry, 0, len(uniquePaths))
	for _, p := range uniquePaths {
		instances := append([]string(nil), pathToInstances[p]...)
		sort.Strings(instances)
		f, ok := pathToFetched[p]
		entry := moduleEntry{
			InferredNames: instances,
			RegistryPath:  p,
			PinnedVersion: "unknown",
			Status:        "check_recommended",
		}
		if !ok || f.bad || f.mod == nil {
			entry.LatestVersion = "unavailable"
		} else {
			entry.LatestVersion = f.mod.Version
			entry.Description = f.mod.Description
			entry.DocsURL = f.mod.DocsURL
		}
		entries = append(entries, entry)
	}

	if unknown == nil {
		unknown = []string{}
	}

	payload := map[string]any{
		"workspace":        workspace,
		"org":              org,
		"modules_detected": len(entries),
		"modules":          entries,
		"unknown_modules":  unknown,
		"note":             moduleAuditNote,
	}

	body, mErr := json.Marshal(payload)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	return result
}

// providerAuditNote is appended to every _hcp_tf_provider_audit response so
// the caller knows where pinned-version data came from. The audit infers
// pinned versions from the workspace's most recent plan-export bundle (the
// `required_providers` constraint block); range constraints stay "unknown"
// because they don't resolve to a single version. CVE data is OSV.dev.
const providerAuditNote = "Pinned versions are inferred from the required_providers constraint block in the most recent plan export. Exact constraints (e.g. \"4.9.0\") are reported as pinned_version; range constraints (~>, >=, etc.) leave pinned_version: unknown. CVE data sourced from OSV.dev."

// providerRef captures a single provider extracted from a workspace state or
// resource list. Namespace and Name are decoded from the registry path; Raw is
// the full provider address as it appeared in the source so we can surface
// non-hashicorp providers verbatim under unknown_providers.
type providerRef struct {
	Raw       string
	Namespace string
	Name      string
}

// providerRegistryEntry mirrors the JSON shape of `hcptf publicregistry provider`.
type providerRegistryEntry struct {
	Name        string `json:"Name"`
	Version     string `json:"Version"`
	Description string `json:"Description"`
	DocsURL     string `json:"DocsURL"`
	Source      string `json:"Source"`
}

// parseProviderAddress decodes the per-resource provider field shape
// `provider["registry.terraform.io/<ns>/<name>"]` (or the bare path with no
// brackets) into a providerRef. Returns false when the address can't be
// parsed; the caller should treat those as unknown_providers.
func parseProviderAddress(addr string) (providerRef, bool) {
	s := strings.TrimSpace(addr)
	if s == "" {
		return providerRef{}, false
	}
	// Strip the optional `provider["..."]` wrapper.
	if strings.HasPrefix(s, "provider[") && strings.HasSuffix(s, "]") {
		inner := s[len("provider[") : len(s)-1]
		inner = strings.Trim(inner, `"`)
		s = inner
	}
	// Strip leading registry host so we can split on namespace/name.
	for _, host := range []string{"registry.terraform.io/", "registry.opentofu.org/"} {
		if strings.HasPrefix(s, host) {
			s = strings.TrimPrefix(s, host)
			break
		}
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return providerRef{Raw: addr}, false
	}
	return providerRef{Raw: addr, Namespace: parts[0], Name: parts[1]}, true
}

// extractProvidersFromStateJSON walks a downloaded state JSON and returns the
// distinct provider references seen across all resources. Order is sorted by
// namespace/name for stable output. Returns (refs, unknownAddresses, err).
func extractProvidersFromStateJSON(raw []byte) ([]providerRef, []string, error) {
	var state struct {
		Resources []struct {
			Provider string `json:"provider"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, nil, err
	}
	seen := map[string]providerRef{}
	unknownSeen := map[string]struct{}{}
	var unknown []string
	for _, r := range state.Resources {
		addr := strings.TrimSpace(r.Provider)
		if addr == "" {
			continue
		}
		ref, ok := parseProviderAddress(addr)
		if !ok {
			if _, dup := unknownSeen[addr]; !dup {
				unknownSeen[addr] = struct{}{}
				unknown = append(unknown, addr)
			}
			continue
		}
		key := ref.Namespace + "/" + ref.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = ref
	}
	refs := make([]providerRef, 0, len(seen))
	for _, v := range seen {
		refs = append(refs, v)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		return refs[i].Name < refs[j].Name
	})
	sort.Strings(unknown)
	return refs, unknown, nil
}

// extractProvidersFromResources is the fallback path when state download fails.
// hcptf workspace resource list surfaces a "provider_name" field per resource
// in the same provider["..."] format used inside state files.
func extractProvidersFromResources(items []workspaceResource) ([]providerRef, []string) {
	seen := map[string]providerRef{}
	unknownSeen := map[string]struct{}{}
	var unknown []string
	for _, it := range items {
		addr := strings.TrimSpace(it.Provider)
		if addr == "" {
			continue
		}
		ref, ok := parseProviderAddress(addr)
		if !ok {
			if _, dup := unknownSeen[addr]; !dup {
				unknownSeen[addr] = struct{}{}
				unknown = append(unknown, addr)
			}
			continue
		}
		key := ref.Namespace + "/" + ref.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = ref
	}
	refs := make([]providerRef, 0, len(seen))
	for _, v := range seen {
		refs = append(refs, v)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		return refs[i].Name < refs[j].Name
	})
	sort.Strings(unknown)
	return refs, unknown
}

// fetchProviderRegistryEntry shells out to `hcptf publicregistry provider` for
// a single namespace/name path and returns the parsed metadata or a sentinel
// "unavailable" flag on any failure.
func fetchProviderRegistryEntry(ctx context.Context, registryPath string, timeoutSec int) (*providerRegistryEntry, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "hcptf", "publicregistry", "provider",
		"-name="+registryPath,
		"-output=json",
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		return nil, true
	}
	if looksLikeHTML(string(out)) {
		return nil, true
	}
	var entry providerRegistryEntry
	if err := json.Unmarshal(out, &entry); err != nil {
		return nil, true
	}
	if entry.Version == "" {
		return nil, true
	}
	return &entry, false
}

// fetchOSVProviderAdvisories POSTs a provider package to OSV.dev /v1/query
// without a version field — so the response surfaces every known CVE for the
// package, framed as what an upgrade would address. Mirrors fetchOSVAdvisories
// but with the provider package name and no version filter.
func fetchOSVProviderAdvisories(ctx context.Context, providerShortName string, timeoutSec int) ([]advisoryEntry, bool) {
	perQuery := 10 * time.Second
	if d := time.Duration(timeoutSec) * time.Second; d > 0 && d < perQuery {
		perQuery = d
	}
	cctx, cancel := context.WithTimeout(ctx, perQuery)
	defer cancel()

	pkgName := "github.com/hashicorp/terraform-provider-" + providerShortName
	body := []byte(fmt.Sprintf(`{"package":{"name":%q,"ecosystem":"Go"}}`, pkgName))
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, "https://api.osv.dev/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tfpilot/1.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, true
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, true
	}
	if looksLikeHTML(string(raw)) {
		return nil, true
	}
	entries, parseErr := parseOSVResponse(raw)
	if parseErr {
		return nil, true
	}
	return entries, false
}

// downloadWorkspaceState shells out to `hcptf state download` and returns the
// raw state JSON bytes. Returns (nil, ferr) when the download fails — the
// caller is responsible for falling back to resource-address extraction.
func downloadWorkspaceState(ctx context.Context, org, workspace string, timeoutSec int) ([]byte, *ToolError) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "hcptf", "state", "download",
		"-org="+org,
		"-workspace="+workspace,
	)
	out, execErr := cmd.Output()
	if execErr != nil {
		stderr := ""
		if e, ok := execErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(e.Stderr))
		}
		if looksLikeHTML(string(out)) || looksLikeHTML(stderr) {
			return nil, htmlGuardError()
		}
		msg := execErr.Error()
		if stderr != "" {
			msg = stderr
		}
		return nil, &ToolError{ErrorCode: "execution_error", Message: msg, Retryable: true}
	}
	if looksLikeHTML(string(out)) {
		return nil, htmlGuardError()
	}
	return out, nil
}

// providerEntryRE pairs a provider's `full_name` with its `version_constraint`
// inside the providers block. The non-greedy `.*?` between the two keys keeps
// each block self-contained.
var providerEntryRE = regexp.MustCompile(`(?s)"full_name"\s*:\s*"([^"]+)".*?"version_constraint"\s*:\s*"([^"]*)"`)

// providerBlockHeaderRE matches the line that opens the top-level
// `providers = {` block in a Sentinel mock-tfconfig-v2 file. We isolate this
// block before applying providerEntryRE because module_calls in the same
// file also uses the key `version_constraint` (to describe module versions,
// not provider versions).
var providerBlockHeaderRE = regexp.MustCompile(`^providers\s*=\s*\{\s*$`)

// fetchProviderConstraintsFromPlanExport tries to recover per-provider
// `required_providers` constraints from the workspace's most recent plan
// export (sentinel-mock-bundle-v0). Returns map[registryPath]constraint, e.g.
// {"hashicorp/aws": "~> 4.45.0"}. Returns nil on any failure — callers treat
// absence as "no constraint information" and leave pinned_version "unknown".
//
// The probe orchestrates several sequential hcptf shell-outs plus a poll for
// async export completion, so it gets its own 60s deadline regardless of the
// per-call timeoutSec (which is sized for individual shell-outs). Caller ctx
// cancellation still wins via the parent context.
func fetchProviderConstraintsFromPlanExport(ctx context.Context, org, workspace string, timeoutSec int) map[string]string {
	_ = timeoutSec // probe paces itself; param kept for signature parity with siblings
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	wsCmd := exec.CommandContext(cctx, "hcptf", "workspace", "read",
		"-org="+org, "-name="+workspace, "-include=current_run", "-output=json")
	wsOut, err := wsCmd.Output()
	if err != nil || looksLikeHTML(string(wsOut)) {
		return nil
	}
	var ws struct {
		CurrentRunID string `json:"CurrentRunID"`
	}
	if err := json.Unmarshal(wsOut, &ws); err != nil || ws.CurrentRunID == "" {
		return nil
	}

	runCmd := exec.CommandContext(cctx, "hcptf", "run", "show",
		"-id="+ws.CurrentRunID, "-output=json")
	runOut, err := runCmd.Output()
	if err != nil || looksLikeHTML(string(runOut)) {
		return nil
	}
	var run struct {
		PlanID string `json:"PlanID"`
	}
	if err := json.Unmarshal(runOut, &run); err != nil || run.PlanID == "" {
		return nil
	}

	expCmd := exec.CommandContext(cctx, "hcptf", "planexport", "create",
		"-plan-id="+run.PlanID, "-output=json")
	expOut, err := expCmd.Output()
	var exp struct {
		ID string `json:"ID"`
	}
	switch {
	case err == nil && !looksLikeHTML(string(expOut)):
		// `planexport create` prefixes a "Plan export 'pe-...' created successfully"
		// line before the JSON; trim to the opening brace.
		if i := bytes.IndexByte(expOut, '{'); i > 0 {
			expOut = expOut[i:]
		}
		if uerr := json.Unmarshal(expOut, &exp); uerr != nil || exp.ID == "" {
			return nil
		}
	case err != nil:
		// TFC permits only one pending/downloadable export per plan per data
		// type. When a previous audit (in this or another session) crashed
		// before the cleanup defer ran, the next create returns "Plan already
		// has a pending or downloadable export of this type". Recover the
		// existing export id via the JSON API since hcptf surfaces no list
		// command, then reuse it; the defer below still cleans up afterwards.
		stderr := ""
		if e, ok := err.(*exec.ExitError); ok {
			stderr = string(e.Stderr)
		}
		if !strings.Contains(stderr, "Plan already has") {
			return nil
		}
		existing := lookupExistingPlanExport(cctx, run.PlanID)
		if existing == "" {
			return nil
		}
		exp.ID = existing
	default:
		return nil
	}
	// Best-effort cleanup: delete the export when we're done with it. Detached
	// context so the deletion still runs even if the parent ctx is mid-
	// cancellation. Without this, the next audit invocation hits "Plan already
	// has a pending or downloadable export" and falls back to the API lookup.
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer delCancel()
		delCmd := exec.CommandContext(delCtx, "hcptf", "planexport", "delete",
			"-id="+exp.ID, "-y")
		_ = delCmd.Run()
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		readCmd := exec.CommandContext(cctx, "hcptf", "planexport", "read",
			"-id="+exp.ID, "-output=json")
		readOut, rerr := readCmd.Output()
		if rerr != nil {
			return nil
		}
		var status struct {
			Status string `json:"Status"`
		}
		if uerr := json.Unmarshal(readOut, &status); uerr != nil {
			return nil
		}
		if status.Status == "finished" {
			break
		}
		if status.Status == "errored" || status.Status == "expired" || status.Status == "canceled" {
			return nil
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	tmpDir, err := os.MkdirTemp("", "tfpilot-pe-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmpDir)
	archive := filepath.Join(tmpDir, "pe.tar.gz")
	dlCmd := exec.CommandContext(cctx, "hcptf", "planexport", "download",
		"-id="+exp.ID, "-path="+archive)
	if _, err := dlCmd.Output(); err != nil {
		return nil
	}

	body, err := readSentinelTfconfigV2(archive)
	if err != nil {
		return nil
	}
	return parseProviderConstraints(body)
}

// lookupExistingPlanExport fetches a plan via the TFC HTTP API to recover the
// ID of an existing plan-export. Used as a fallback when `hcptf planexport
// create` returns "Plan already has a pending or downloadable export" — the
// hcptf CLI surfaces no list-by-plan command for plan exports, but the JSON
// API exposes the relationship under data.relationships.exports.
//
// Returns "" on any failure. The caller treats absence as "no recovery
// possible" and degrades pinned_version_source to "unknown".
func lookupExistingPlanExport(ctx context.Context, planID string) string {
	token := readTFCToken()
	if token == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		"https://app.terraform.io/api/v2/plans/"+planID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot/1.5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var doc struct {
		Data struct {
			Relationships struct {
				Exports struct {
					Data []struct {
						ID   string `json:"id"`
						Type string `json:"type"`
					} `json:"data"`
				} `json:"exports"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, e := range doc.Data.Relationships.Exports.Data {
		if e.Type == "plan-exports" && e.ID != "" {
			return e.ID
		}
	}
	return ""
}

// readTFCToken reads the bearer token from the standard Terraform credentials
// file at ~/.terraform.d/credentials.tfrc.json. Returns "" when the file is
// missing or malformed; callers fall back to whatever read-only path remains.
func readTFCToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".terraform.d", "credentials.tfrc.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		Credentials map[string]struct {
			Token string `json:"token"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return ""
	}
	if c, ok := creds.Credentials["app.terraform.io"]; ok {
		return c.Token
	}
	return ""
}

// readSentinelTfconfigV2 untars a Sentinel mock bundle (.tar.gz) and returns
// the body of mock-tfconfig-v2.sentinel — the file that carries the
// `required_providers` constraint block.
func readSentinelTfconfigV2(archivePath string) ([]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			return nil, fmt.Errorf("mock-tfconfig-v2.sentinel not found")
		}
		if terr != nil {
			return nil, terr
		}
		if filepath.Base(hdr.Name) == "mock-tfconfig-v2.sentinel" {
			return io.ReadAll(tr)
		}
	}
}

// parseProviderConstraints walks the `providers = {...}` block of a
// mock-tfconfig-v2.sentinel body and returns a map of registryPath to
// version_constraint string. Returns nil when no providers block is found.
//
// The block is isolated by line-scan: the opening line is `providers = {`,
// and the close is a `}` at column zero (Sentinel mock files emit each
// top-level binding's close brace flush left). Anything between is fed to
// providerEntryRE to pair full_name with version_constraint.
func parseProviderConstraints(body []byte) map[string]string {
	lines := strings.Split(string(body), "\n")
	startIdx := -1
	for i, line := range lines {
		if providerBlockHeaderRE.MatchString(line) {
			startIdx = i + 1
			break
		}
	}
	if startIdx < 0 {
		return nil
	}
	endIdx := -1
	for i := startIdx; i < len(lines); i++ {
		if lines[i] == "}" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		return nil
	}
	block := []byte(strings.Join(lines[startIdx:endIdx], "\n"))
	out := map[string]string{}
	for _, match := range providerEntryRE.FindAllSubmatch(block, -1) {
		fullName := string(match[1])
		constraint := strings.TrimSpace(string(match[2]))
		for _, host := range []string{"registry.terraform.io/", "registry.opentofu.org/"} {
			fullName = strings.TrimPrefix(fullName, host)
		}
		if fullName == "" || strings.Count(fullName, "/") != 1 {
			continue
		}
		out[fullName] = constraint
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveExactConstraint returns (version, true) when the constraint string
// reduces to a single literal version (e.g. "4.9.0", "= 4.9.0", "v4.9.0").
// Range operators (~>, >=, <=, <, >, !=) and compound constraints yield
// ("", false) — those forms don't pin to a single version, so we leave the
// audit's pinned_version as "unknown".
func resolveExactConstraint(c string) (string, bool) {
	s := strings.TrimSpace(c)
	if s == "" {
		return "", false
	}
	for _, op := range []string{"~>", ">=", "<=", "!=", ">", "<"} {
		if strings.HasPrefix(s, op) {
			return "", false
		}
	}
	for _, prefix := range []string{"==", "=", "v", "V"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	if strings.ContainsAny(s, " \t,*~!<>=") {
		return "", false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", false
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return "", false
			}
		}
	}
	return s, true
}

// computeCVEDiff partitions the full CVE list for a provider into the two
// slices the audit surfaces:
//   - currentlyAffected: pinned predates fixed_in (or fix unknown).
//   - upgradingFixes:    fixed_in is between pinned and latest, inclusive of
//     latest.
//
// Both slices are non-nil so JSON marshals as `[]` rather than `null`. Only
// called when both pinned and latest are real versions; the unknown-pinned
// path bypasses this and copies all CVEs into upgradingFixes.
func computeCVEDiff(pinned, latest string, cves []advisoryEntry) (currentlyAffected, upgradingFixes []advisoryEntry) {
	currentlyAffected = []advisoryEntry{}
	upgradingFixes = []advisoryEntry{}
	for _, c := range cves {
		if c.FixedIn == "" || compareSemver(pinned, c.FixedIn) < 0 {
			currentlyAffected = append(currentlyAffected, c)
		}
		if c.FixedIn != "" &&
			compareSemver(pinned, c.FixedIn) < 0 &&
			compareSemver(c.FixedIn, latest) <= 0 {
			upgradingFixes = append(upgradingFixes, c)
		}
	}
	return currentlyAffected, upgradingFixes
}

// buildProviderUpgradeNote writes the plain-English upgrade summary for a
// single provider entry. The agent's system prompt instructs it to surface
// this verbatim, so the wording carries the operator-facing explanation of
// what pinned_version, currently_affected, and upgrading_fixes actually mean.
func buildProviderUpgradeNote(pinned, latest string, allCVEs, currentlyAffected, upgradingFixes int) string {
	if latest == "unavailable" {
		return "Could not determine latest version from the registry."
	}
	if pinned == "unknown" {
		if allCVEs == 0 {
			return fmt.Sprintf("No known CVEs for this provider. Pinned version is unknown — check .terraform.lock.hcl. Upgrading to %s is still recommended to stay current.", latest)
		}
		return fmt.Sprintf("Pinned version is unknown — upgrading to %s addresses all %d known CVE%s. Check .terraform.lock.hcl to determine which apply to your current version.", latest, allCVEs, plural(allCVEs))
	}
	if compareSemver(pinned, latest) >= 0 {
		if allCVEs == 0 {
			return fmt.Sprintf("Pinned at %s, which is the latest. No known CVEs for this provider.", pinned)
		}
		if currentlyAffected == 0 {
			return fmt.Sprintf("Pinned at %s, which is the latest. None of the %d known CVE%s affect this version.", pinned, allCVEs, plural(allCVEs))
		}
		return fmt.Sprintf("Pinned at %s; %d known CVE%s still affect this version with no published fix yet.", pinned, currentlyAffected, plural(currentlyAffected))
	}
	if upgradingFixes == 0 && currentlyAffected == 0 {
		return fmt.Sprintf("Pinned at %s; latest is %s. None of the %d known CVE%s affect this version.", pinned, latest, allCVEs, plural(allCVEs))
	}
	if upgradingFixes == 0 {
		return fmt.Sprintf("Pinned at %s; latest is %s. %d CVE%s affect this version but no fix has been published yet.", pinned, latest, currentlyAffected, plural(currentlyAffected))
	}
	return fmt.Sprintf("Pinned at %s; upgrading to %s would resolve %d known CVE%s.", pinned, latest, upgradingFixes, plural(upgradingFixes))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// providerAuditCall extracts the providers used by a workspace, infers
// pinned versions from the most recent plan export's required_providers
// block, fetches the latest registry version, queries OSV.dev for every
// known CVE per provider, and partitions those CVEs into currently_affected
// and upgrading_fixes when pinned is an exact version. Read-only.
func providerAuditCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_provider_audit", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	// Primary path: download state JSON and extract providers from it.
	// Fallback: parse provider_name fields from `hcptf workspace resource list`
	// when the state download fails (private S3 redirect, plan-tier gate, etc.).
	var refs []providerRef
	var unknown []string
	stateDownloadFailed := false
	stateRaw, dlErr := downloadWorkspaceState(ctx, org, workspace, timeoutSec)
	if dlErr == nil {
		parsed, unk, perr := extractProvidersFromStateJSON(stateRaw)
		if perr != nil {
			stateDownloadFailed = true
		} else {
			refs = parsed
			unknown = unk
		}
	} else {
		stateDownloadFailed = true
	}
	if stateDownloadFailed {
		raw, fetchErr := fetchWorkspaceResources(ctx, org, workspace, timeoutSec)
		if fetchErr != nil {
			result.Err = fetchErr
			result.Duration = time.Since(start)
			return result
		}
		items, perr := unmarshalResources(raw)
		if perr != nil {
			result.Err = &ToolError{ErrorCode: "parse_error", Message: perr.Error()}
			result.Duration = time.Since(start)
			return result
		}
		refs, unknown = extractProvidersFromResources(items)
	}

	type knownProvider struct {
		ref          providerRef
		registryPath string
	}
	var known []knownProvider
	for _, r := range refs {
		if r.Namespace != "hashicorp" {
			unknown = append(unknown, r.Raw)
			continue
		}
		known = append(known, knownProvider{ref: r, registryPath: r.Namespace + "/" + r.Name})
	}
	sort.Strings(unknown)

	// Probe the most recent plan export for required_providers constraints
	// in parallel with the OSV/registry fan-out below. The probe is a
	// multi-step async flow (workspace read → run show → planexport create →
	// poll → download → parse) so we don't want it gating OSV calls.
	type probeResult struct {
		constraints map[string]string
		source      string
	}
	probeCh := make(chan probeResult, 1)
	go func() {
		c := fetchProviderConstraintsFromPlanExport(ctx, org, workspace, timeoutSec)
		src := "unknown"
		if len(c) > 0 {
			src = "planexport"
		}
		probeCh <- probeResult{constraints: c, source: src}
	}()

	// Fan out registry + OSV lookups with bounded concurrency.
	type fetched struct {
		idx    int
		entry  *providerRegistryEntry
		regBad bool
		cves   []advisoryEntry
		cveBad bool
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	results := make([]fetched, 0, len(known))
	for i, k := range known {
		wg.Add(1)
		go func(idx int, kp knownProvider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entry, regBad := fetchProviderRegistryEntry(ctx, kp.registryPath, timeoutSec)
			cves, cveBad := fetchOSVProviderAdvisories(ctx, kp.ref.Name, timeoutSec)
			mu.Lock()
			results = append(results, fetched{idx: idx, entry: entry, regBad: regBad, cves: cves, cveBad: cveBad})
			mu.Unlock()
		}(i, k)
	}
	wg.Wait()
	idxToFetched := make(map[int]fetched, len(results))
	cveDataUnavailable := false
	for _, r := range results {
		idxToFetched[r.idx] = r
		if r.cveBad {
			cveDataUnavailable = true
		}
	}

	// Reap the probe goroutine started above. The fan-out usually outlives
	// the probe in workspaces with many providers, but on small workspaces
	// the probe can take longer than the OSV calls.
	probe := <-probeCh
	constraints := probe.constraints
	pinnedSource := probe.source

	type providerEntry struct {
		Name              string          `json:"name"`
		Namespace         string          `json:"namespace"`
		RegistryPath      string          `json:"registry_path"`
		PinnedVersion     string          `json:"pinned_version"`
		VersionConstraint string          `json:"version_constraint"`
		LatestVersion     string          `json:"latest_version"`
		AllCVEs           []advisoryEntry `json:"all_cves"`
		CurrentlyAffected []advisoryEntry `json:"currently_affected"`
		UpgradingFixes    []advisoryEntry `json:"upgrading_fixes"`
		CVECount          int             `json:"cve_count"`
		Status            string          `json:"status"`
		UpgradeNote       string          `json:"upgrade_note"`
	}
	entries := make([]providerEntry, 0, len(known))
	for i, k := range known {
		f := idxToFetched[i]
		latest := "unavailable"
		if !f.regBad && f.entry != nil {
			latest = f.entry.Version
		}
		cves := f.cves
		if cves == nil {
			cves = []advisoryEntry{}
		}

		constraint := constraints[k.registryPath]
		pinned := "unknown"
		if v, ok := resolveExactConstraint(constraint); ok {
			pinned = v
		}

		var currentlyAffected, upgradingFixes []advisoryEntry
		if pinned != "unknown" && latest != "unavailable" {
			currentlyAffected, upgradingFixes = computeCVEDiff(pinned, latest, cves)
		} else {
			currentlyAffected = []advisoryEntry{}
			upgradingFixes = append([]advisoryEntry{}, cves...)
		}

		status := "check_recommended"
		if pinned != "unknown" && latest != "unavailable" {
			switch {
			case len(currentlyAffected) > 0:
				status = "needs_upgrade"
			case compareSemver(pinned, latest) >= 0:
				status = "current"
			default:
				status = "needs_upgrade"
			}
		}

		note := buildProviderUpgradeNote(pinned, latest, len(cves), len(currentlyAffected), len(upgradingFixes))

		entries = append(entries, providerEntry{
			Name:              k.ref.Name,
			Namespace:         k.ref.Namespace,
			RegistryPath:      k.registryPath,
			PinnedVersion:     pinned,
			VersionConstraint: constraint,
			LatestVersion:     latest,
			AllCVEs:           cves,
			CurrentlyAffected: currentlyAffected,
			UpgradingFixes:    upgradingFixes,
			CVECount:          len(cves),
			Status:            status,
			UpgradeNote:       note,
		})
	}

	if unknown == nil {
		unknown = []string{}
	}

	payload := map[string]any{
		"org":                   org,
		"workspace":             workspace,
		"providers":             entries,
		"unknown_providers":     unknown,
		"cve_data_unavailable":  cveDataUnavailable,
		"state_download_failed": stateDownloadFailed,
		"pinned_version_source": pinnedSource,
		"note":                  providerAuditNote,
	}

	body, mErr := json.Marshal(payload)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(body)
	result.Duration = time.Since(start)
	return result
}

// normalizeTerraformVersion strips constraint prefixes and the leading "v" so
// values like "~>1.14.0", ">= 1.5.0", or "v1.5.0" become "1.14.0" / "1.5.0".
// hcptf surfaces both pinned versions and Terraform CLI constraint operators
// in the same field; we collapse to the underlying version for OSV lookups.
func normalizeTerraformVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, prefix := range []string{"~>", ">=", "<=", "==", "=", "<", ">", "v", "V"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	return s
}

// fetchOSVAdvisories POSTs a single Terraform version to OSV.dev /v1/query.
// Returns (entries, false) on success, (nil, true) when OSV is unreachable or
// returns an unparseable body. Server-side version matching means the response
// only contains vulns affecting the queried version, so no client-side range
// parser is needed.
func fetchOSVAdvisories(ctx context.Context, version string, timeoutSec int) ([]advisoryEntry, bool) {
	// Per-query budget capped at 10s — OSV is fast and we'd rather degrade
	// gracefully on a stuck query than hold the whole audit hostage.
	perQuery := 10 * time.Second
	if d := time.Duration(timeoutSec) * time.Second; d > 0 && d < perQuery {
		perQuery = d
	}
	cctx, cancel := context.WithTimeout(ctx, perQuery)
	defer cancel()

	body := []byte(fmt.Sprintf(`{"version":%q,"package":{"name":"github.com/hashicorp/terraform","ecosystem":"Go"}}`, version))
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, "https://api.osv.dev/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tfpilot/1.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, true
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, true
	}
	if looksLikeHTML(string(raw)) {
		return nil, true
	}
	entries, parseErr := parseOSVResponse(raw)
	if parseErr {
		return nil, true
	}
	return entries, false
}

// parseOSVResponse extracts advisoryEntry values from an OSV.dev /v1/query
// response body. It tolerates partially populated vulns (no severity, no fix
// event) without erroring; only a JSON parse failure on the outer wrapper
// reports an error. Split out from fetchOSVAdvisories so tests can exercise
// the parsing logic without hitting the network.
func parseOSVResponse(raw []byte) ([]advisoryEntry, bool) {
	var wrapper struct {
		Vulns []struct {
			ID       string   `json:"id"`
			Summary  string   `json:"summary"`
			Aliases  []string `json:"aliases"`
			Database struct {
				Severity string `json:"severity"`
			} `json:"database_specific"`
			Affected []struct {
				Ranges []struct {
					Events []map[string]string `json:"events"`
				} `json:"ranges"`
			} `json:"affected"`
		} `json:"vulns"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, true
	}

	out := make([]advisoryEntry, 0, len(wrapper.Vulns))
	seen := map[string]bool{}
	for _, v := range wrapper.Vulns {
		id := v.ID
		// Prefer the canonical CVE id when present in aliases — it's more
		// recognizable to operators than a GHSA-* or GO-* id.
		for _, alias := range v.Aliases {
			if strings.HasPrefix(alias, "CVE-") {
				id = alias
				break
			}
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		fixed := ""
		for _, aff := range v.Affected {
			for _, r := range aff.Ranges {
				for _, ev := range r.Events {
					if f, ok := ev["fixed"]; ok && f != "" {
						if fixed == "" || compareSemver(f, fixed) < 0 {
							fixed = f
						}
					}
				}
			}
		}

		out = append(out, advisoryEntry{
			ID:       id,
			Summary:  v.Summary,
			Severity: normalizeSeverity(v.Database.Severity),
			FixedIn:  fixed,
		})
	}
	return out, false
}

// normalizeSeverity maps OSV's database_specific.severity values
// (LOW/MODERATE/MEDIUM/HIGH/CRITICAL) to tfpilot's lowercase taxonomy.
// Empty or unrecognized inputs become "unknown".
func normalizeSeverity(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "LOW":
		return "low"
	case "MODERATE", "MEDIUM":
		return "medium"
	case "HIGH":
		return "high"
	case "CRITICAL":
		return "critical"
	default:
		return "unknown"
	}
}

// compareSemver returns -1, 0, or 1 for a vs b on a 3-component dotted version
// (major.minor.patch). Any parse failure on a component degrades to lexical
// compare on that component, which preserves a sane ordering for "unknown" or
// truncated versions without erroring.
func compareSemver(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var ai, bi int
		var aerr, berr error
		if i < len(pa) {
			ai, aerr = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			bi, berr = strconv.Atoi(pb[i])
		}
		if aerr != nil || berr != nil {
			as := ""
			bs := ""
			if i < len(pa) {
				as = pa[i]
			}
			if i < len(pb) {
				bs = pb[i]
			}
			if as < bs {
				return -1
			}
			if as > bs {
				return 1
			}
			continue
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

func majorComponent(v string) int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) == 0 {
		return -1
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1
	}
	return n
}

func minorComponent(v string) int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return -1
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return n
}

// versionsBehind returns the minor-version delta between current and latest
// (latest.minor - current.minor). Returns 0 when current is unparseable so
// a missing version doesn't get scored as catastrophically behind by accident.
func versionsBehind(current, latest string) int {
	cm := minorComponent(current)
	lm := minorComponent(latest)
	if cm < 0 || lm < 0 {
		return 0
	}
	delta := lm - cm
	if delta < 0 {
		return 0
	}
	return delta
}

// upgradeComplexity scores per-version-group upgrade effort. Inputs are the
// max workspace resource count in the group, the minor-version delta from
// latest, and whether a major bump is involved. Scale matches the v1.5 spec:
// Low (<10 resources, <2 behind), Medium (10–50 OR 2–5 behind),
// High (>50 OR major jump OR >5 behind).
func upgradeComplexity(resources, behind int, majorJump bool) string {
	if resources > 50 || majorJump || behind > 5 {
		return "High"
	}
	if resources >= 10 || behind >= 2 {
		return "Medium"
	}
	return "Low"
}

func highestSeverity(cves []advisoryEntry) string {
	rank := map[string]int{"unknown": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}
	highest := "unknown"
	for _, c := range cves {
		if rank[c.Severity] > rank[highest] {
			highest = c.Severity
		}
	}
	return highest
}

func upgradeNotes(current, latest string, behind int, majorJump bool, cveCount int) string {
	if current == "unknown" {
		return "Workspace has no Terraform version pinned — set one explicitly."
	}
	if majorJump {
		return fmt.Sprintf("Major version bump from %s to %s. Review the upgrade guide and re-test all modules.", current, latest)
	}
	if behind >= 5 {
		return fmt.Sprintf("More than 5 minor versions behind %s. Plan an incremental upgrade through the intermediate releases.", latest)
	}
	if behind >= 2 {
		return fmt.Sprintf("%d minor versions behind %s. Upgrade should be straightforward but re-run plans on a non-prod workspace first.", behind, latest)
	}
	if cveCount > 0 {
		return fmt.Sprintf("Within range of %s but a known CVE applies. Patch by upgrading to the listed fixed version.", latest)
	}
	return fmt.Sprintf("Within 2 minor versions of %s. No urgent action.", latest)
}

func buildRecommendation(summaries []summaryEntry, cveDataUnavailable bool) string {
	criticalWithCVE := summaryEntry{}
	criticalNoCVE := summaryEntry{}
	outdated := summaryEntry{}
	hasCriticalWithCVE := false
	hasCriticalNoCVE := false
	hasOutdated := false

	for _, s := range summaries {
		if s.Status == "critical" && s.CVECount > 0 && !hasCriticalWithCVE {
			criticalWithCVE = s
			hasCriticalWithCVE = true
		}
		if s.Status == "critical" && s.CVECount == 0 && !hasCriticalNoCVE {
			criticalNoCVE = s
			hasCriticalNoCVE = true
		}
		if s.Status == "outdated" && !hasOutdated {
			outdated = s
			hasOutdated = true
		}
	}

	var msg string
	switch {
	case hasCriticalWithCVE:
		msg = fmt.Sprintf("Upgrade %s from %s to address %d known CVE(s) including %s.",
			strings.Join(criticalWithCVE.Workspaces, ", "),
			criticalWithCVE.TerraformVersion,
			criticalWithCVE.CVECount,
			highestSeverityID(criticalWithCVE.KnownCVEs))
	case hasCriticalNoCVE:
		msg = fmt.Sprintf("%d workspace(s) are running Terraform %s, more than 5 minor versions behind %s. Plan an upgrade path.",
			criticalNoCVE.WorkspaceCount, criticalNoCVE.TerraformVersion, latestTerraformVersion)
	case hasOutdated:
		msg = fmt.Sprintf("%s on %s are behind. Upgrade when convenient.",
			strings.Join(outdated.Workspaces, ", "), outdated.TerraformVersion)
	default:
		msg = fmt.Sprintf("All workspaces within 2 minor versions of %s. No urgent action.", latestTerraformVersion)
	}

	if cveDataUnavailable {
		if hasCriticalWithCVE {
			msg += " Note: some OSV.dev queries did not complete; CVE coverage may be incomplete on other versions."
		} else {
			msg += " CVE data unavailable — OSV.dev unreachable."
		}
	}
	return msg
}

func highestSeverityID(cves []advisoryEntry) string {
	rank := map[string]int{"unknown": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}
	bestID := ""
	best := -1
	for _, c := range cves {
		if rank[c.Severity] > best {
			best = rank[c.Severity]
			bestID = c.ID
		}
	}
	return bestID
}

type summaryEntry struct {
	TerraformVersion  string          `json:"terraform_version"`
	WorkspaceCount    int             `json:"workspace_count"`
	Workspaces        []string        `json:"workspaces"`
	Status            string          `json:"status"`
	VersionsBehind    int             `json:"versions_behind"`
	KnownCVEs         []advisoryEntry `json:"known_cves"`
	CVECount          int             `json:"cve_count"`
	UpgradeComplexity string          `json:"upgrade_complexity"`
	UpgradeNotes      string          `json:"upgrade_notes"`
}

// stacksListCall shells out to `hcptf stack list` and enriches each stack
// with deployment_count and health by fanning out parallel
// `stack deployment list` calls. The base `stack list` payload only includes
// id/name/project/created — without the fan-out, /stacks would always render
// "0 deployments" and "Unknown" health. Read-only; visible in every mode.
func stacksListCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_stacks_list", Args: args}

	if err := require(args, "org"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "stack", "list", "-org="+args["org"], "-output=json")
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}

	var stacks []map[string]any
	if err := json.Unmarshal(raw, &stacks); err != nil {
		// Pass the raw payload through if it's not the expected shape so the
		// agent at least sees what hcptf returned.
		result.Output = json.RawMessage(raw)
		result.Duration = time.Since(start)
		return result
	}

	var wg sync.WaitGroup
	for i := range stacks {
		stack := stacks[i]
		stackID := firstStringField(stack, "id", "ID")
		if stackID == "" {
			stack["deployment_count"] = 0
			stack["health"] = "Unknown"
			continue
		}
		wg.Add(1)
		go func(s map[string]any, id string) {
			defer wg.Done()
			deployRaw, derr := fetchHCPTFJSON(ctx, timeoutSec, "stack", "deployment", "list", "-stack-id="+id, "-output=json")
			deployments := parseStackDeployments(deployRaw, derr, s)
			s["deployment_count"] = len(deployments)
			s["health"] = stackHealth(deployments)
		}(stack, stackID)
	}
	wg.Wait()

	out, mErr := json.Marshal(stacks)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = json.RawMessage(out)
	result.Duration = time.Since(start)
	return result
}

// stackDescribeCall fires `stack list`, `stack configuration list`, and
// `stack deployment list` in parallel and synthesizes a single view of a
// stack: metadata, configuration count, deployments with status, a computed
// health label, and the invariant limitations of Stacks today.
func stackDescribeCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_stack_describe", Args: args}

	if err := require(args, "org", "stack_id"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	stackID := args["stack_id"]

	type fetchResult struct {
		raw []byte
		err *ToolError
	}
	chStacks := make(chan fetchResult, 1)
	chConfigs := make(chan fetchResult, 1)
	chDeploys := make(chan fetchResult, 1)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "stack", "list", "-org="+org, "-output=json")
		chStacks <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "stack", "configuration", "list", "-stack-id="+stackID, "-output=json")
		chConfigs <- fetchResult{raw: raw, err: ferr}
	}()
	go func() {
		defer wg.Done()
		raw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "stack", "deployment", "list", "-stack-id="+stackID, "-output=json")
		chDeploys <- fetchResult{raw: raw, err: ferr}
	}()
	wg.Wait()
	rStacks := <-chStacks
	rConfigs := <-chConfigs
	rDeploys := <-chDeploys

	if rStacks.err != nil {
		result.Err = rStacks.err
		result.Duration = time.Since(start)
		return result
	}
	if rConfigs.err != nil {
		result.Err = rConfigs.err
		result.Duration = time.Since(start)
		return result
	}
	// rDeploys.err is tolerated — we fall back to the stack-list entry.

	var stacks []map[string]any
	if err := json.Unmarshal(rStacks.raw, &stacks); err != nil {
		result.Err = &ToolError{ErrorCode: "parse_error", Message: "parse stack list: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	var stack map[string]any
	for _, s := range stacks {
		if firstStringField(s, "id", "ID") == stackID {
			stack = s
			break
		}
	}
	if stack == nil {
		result.Err = &ToolError{ErrorCode: "not_found", Message: fmt.Sprintf("stack %s not found in org %s", stackID, org)}
		result.Duration = time.Since(start)
		return result
	}

	name := firstStringField(stack, "name", "Name")
	project := firstStringField(stack, "project", "Project", "project_name", "ProjectName")

	var configs []any
	_ = json.Unmarshal(rConfigs.raw, &configs)

	deployments := parseStackDeployments(rDeploys.raw, rDeploys.err, stack)

	payload := map[string]any{
		"stack_id":            stackID,
		"name":                name,
		"project":             project,
		"configuration_count": len(configs),
		"deployments":         deployments,
		"deployment_count":    len(deployments),
		"health":              stackHealth(deployments),
		"limitations": []string{
			"no policy as code",
			"no drift detection",
			"no run tasks",
		},
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

// parseStackDeployments prefers the dedicated `stack deployment list` response
// and, when that sub-command is unavailable (error or empty payload), falls
// back to any deployments embedded in the `stack list` entry for this stack.
func parseStackDeployments(raw []byte, ferr *ToolError, stack map[string]any) []map[string]any {
	if ferr == nil && len(strings.TrimSpace(string(raw))) > 0 {
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
			out := make([]map[string]any, 0, len(arr))
			for _, d := range arr {
				out = append(out, stackDeploymentShape(d))
			}
			return out
		}
	}
	if stack != nil {
		for _, key := range []string{"deployments", "Deployments"} {
			if arr, ok := stack[key].([]any); ok {
				out := make([]map[string]any, 0, len(arr))
				for _, item := range arr {
					if d, ok := item.(map[string]any); ok {
						out = append(out, stackDeploymentShape(d))
					}
				}
				return out
			}
		}
	}
	return []map[string]any{}
}

func stackDeploymentShape(d map[string]any) map[string]any {
	return map[string]any{
		"name":             firstStringField(d, "name", "Name"),
		"status":           firstStringField(d, "status", "Status"),
		"deployment_group": firstStringField(d, "deployment_group", "DeploymentGroup", "group", "Group"),
		"last_updated":     firstStringField(d, "last_updated", "LastUpdated", "updated_at", "UpdatedAt"),
	}
}

// stackHealth reduces deployment statuses to Healthy | Degraded | Unknown.
// Any errored deployment → Degraded; all applied → Healthy; otherwise
// (empty or mixed/in-flight) → Unknown.
func stackHealth(deployments []map[string]any) string {
	if len(deployments) == 0 {
		return "Unknown"
	}
	for _, d := range deployments {
		status := strings.ToLower(firstStringField(d, "status"))
		if strings.Contains(status, "error") {
			return "Degraded"
		}
	}
	for _, d := range deployments {
		status := strings.ToLower(firstStringField(d, "status"))
		if status != "applied" {
			return "Unknown"
		}
	}
	return "Healthy"
}

// stackKeywords / workspaceKeywords drive _hcp_tf_stack_vs_workspace. Matches
// are case-insensitive substring checks against the user's use_case. When both
// buckets match, workspace wins because policy-as-code, drift detection, and
// run tasks are hard blockers that stacks do not support.
var stackKeywords = []string{
	"multiple environments",
	"repeat",
	"multi-region",
	"region",
	"scale",
	"orchestrat",
	"deploy same",
	"kubernetes",
	"k8s",
	"deferred",
}

var workspaceKeywords = []string{
	"policy",
	"sentinel",
	"opa",
	"drift",
	"run task",
	"explorer",
	"no-code",
	"single environment",
	"simple",
}

// stackVsWorkspaceCall is a pure reasoning tool — no hcptf call. It classifies
// a free-text use case by keyword and returns a recommendation plus the stacks
// limitations that tend to matter for the decision.
func stackVsWorkspaceCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_stack_vs_workspace", Args: args}

	if err := require(args, "org", "use_case"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	_ = ctx
	_ = timeoutSec

	useCase := strings.ToLower(args["use_case"])

	matchedStack := []string{}
	for _, kw := range stackKeywords {
		if strings.Contains(useCase, kw) {
			matchedStack = append(matchedStack, kw)
		}
	}
	matchedWorkspace := []string{}
	for _, kw := range workspaceKeywords {
		if strings.Contains(useCase, kw) {
			matchedWorkspace = append(matchedWorkspace, kw)
		}
	}

	var recommendation, reasoning string
	switch {
	case len(matchedWorkspace) > 0 && len(matchedStack) > 0:
		recommendation = "workspace"
		reasoning = fmt.Sprintf("Matched workspace signal(s) %q and stack signal(s) %q. Workspace wins because policy-as-code, drift detection, and run tasks are hard blockers that stacks do not support today.",
			strings.Join(matchedWorkspace, ", "), strings.Join(matchedStack, ", "))
	case len(matchedWorkspace) > 0:
		recommendation = "workspace"
		reasoning = fmt.Sprintf("Matched workspace signal(s) %q. Use a workspace — stacks do not currently support policy as code, drift detection, or run tasks.",
			strings.Join(matchedWorkspace, ", "))
	case len(matchedStack) > 0:
		recommendation = "stack"
		reasoning = fmt.Sprintf("Matched stack signal(s) %q. Stacks are designed for repeated infrastructure across environments, regions, or accounts, and for deployment orchestration.",
			strings.Join(matchedStack, ", "))
	default:
		recommendation = "either"
		reasoning = "No stack- or workspace-specific signals matched the use case. Either will work; pick workspace when you need policy-as-code, drift detection, or run tasks, or stack when you need to repeat infrastructure across environments or regions."
	}

	payload := map[string]any{
		"recommendation": recommendation,
		"reasoning":      reasoning,
		"use_stack_when": []string{
			"repeated infrastructure across environments, regions, or accounts",
			"deployment orchestration / deferred changes",
			"linked stacks / workload identity",
		},
		"use_workspace_when": []string{
			"policy-as-code required (Sentinel/OPA)",
			"drift detection required",
			"run tasks required",
			"single environment / simple module",
		},
		"key_limitations": []string{
			"Stacks do not support policy as code (Sentinel/OPA)",
			"Stacks do not support drift detection",
			"Stacks do not support run tasks",
			"Maximum 20 deployments per stack",
		},
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
			Name:        "_hcp_tf_org_timeline",
			Description: "Fans out across every workspace in the org with at least one resource and returns the merged run history within the last `hours` (default 24), sorted newest-first. Each entry: { workspace, run_id, status, message, created_at, created_at_human, triggered_by, has_changes, additions, changes, destructions }. Also returns `anomalies[]` with type ∈ { multiple_changes_in_window, repeated_failure, unexpected_destruction, off_hours_change }. Use when the user asks 'what changed', 'what happened', or reports something is wrong in prod and needs cross-workspace correlation. Read-only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":              map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"hours":            map[string]any{"type": "string", "description": "Lookback window in hours (default 24)"},
					"workspace_filter": map[string]any{"type": "string", "description": "Optional comma-separated list of workspace names to restrict the scan to"},
				},
				"required": []string{"org"},
			},
		},
		{
			Name:        "_hcp_tf_rollback",
			Description: "Reverts a workspace to a previous configuration by creating a new run against the previous applied run's configuration version. When `run_id` is omitted, picks the most recent `applied` run other than the current one. The new run is queued for manual apply (auto-apply=false) and reaches `planned` status — the agent must call _hcp_tf_plan_analyze on the resulting run, surface blast radius, and let the user approve via _hcp_tf_run_apply. Returns { workspace, rolled_back_to_run_id, rolled_back_to_created_at, rolled_back_to_human, new_run_id, status, next_step }. Mutating — requires --apply mode.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name to roll back"},
					"run_id":    map[string]any{"type": "string", "description": "Optional explicit run ID to revert to. Must be in `applied` status. When omitted, the most recent applied run other than the current one is used."},
				},
				"required": []string{"org", "workspace"},
			},
		},
		{
			Name:        "_hcp_tf_incident_summary",
			Description: "Generates a postmortem-ready markdown incident report and writes it to ~/.tfpilot/incidents/<YYYY-MM-DD>-<workspace>.md. Pure transformation: pass the JSON output of an earlier _hcp_tf_org_timeline call as `timeline_data`, the JSON output of an earlier _hcp_tf_drift_detect call as `drift_data`, and optionally the `new_run_id` from _hcp_tf_rollback as `rollback_run_id`. The report includes Summary, Timeline table, Root Cause (inferred from drift), Impact, Resolution, and Action Items. Returns { report_path, incident_duration_minutes, root_cause, affected_workspaces, report_markdown }. Read-only — writes to local disk only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":             map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace":       map[string]any{"type": "string", "description": "Primary workspace involved in the incident"},
					"timeline_data":   map[string]any{"type": "string", "description": "JSON string from a prior _hcp_tf_org_timeline call"},
					"drift_data":      map[string]any{"type": "string", "description": "JSON string from a prior _hcp_tf_drift_detect call"},
					"rollback_run_id": map[string]any{"type": "string", "description": "Optional new_run_id returned by _hcp_tf_rollback when a rollback was applied"},
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
			Name:        "_hcp_tf_workspace_ownership",
			Description: "Returns workspace ownership and metadata: created_at and last_updated (with *_human relative strings), vcs_repo (or null), description, team_access (array of {team, access}) with team_access_note, last_modified_by ({username, email} or null) with last_modified_by_note, inferred_owner (the admin team's name, else last modifier's username, else 'unknown'). Read-only.",
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
			Name:        "_hcp_tf_workspace_dependencies",
			Description: "Maps cross-workspace dependencies by walking each workspace's state file for terraform_remote_state data sources. With `workspace`, returns per-workspace { depends_on[], depended_by[], dependency_depth, is_root, is_leaf, note }. Without `workspace`, returns the full org-wide dependency_graph with roots, leaves, and total_dependency_edges. Empty graphs are returned with an explanatory note (not an error) when no terraform_remote_state references exist. Read-only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Workspace name. Optional — omit for an org-wide dependency map."},
				},
				"required": []string{"org"},
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
			Description: "Returns the latest health assessment for a workspace via the JSON:API current-assessment-result endpoint. Output: { workspace, org, assessments_enabled, assessment_id, drifted, succeeded, assessment_status (ok|drifted|error), summary, last_assessment_at, last_assessed_human, resources_drifted, resources_undrifted, drifted_resources[{address,provider,change_type}], drifted_addresses[], error_message }. change_type ∈ { modified, deleted, added, replaced, changed }. When health assessments are not enabled (HTTP 404) returns { assessments_enabled: false, message }. Read-only.",
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
			Name:        "_hcp_tf_run_diagnose",
			Description: "Explains why a run failed. Fetches run details plus plan and apply logs, categorizes the error (auth|quota|resource_conflict|provider|config|policy|network|unknown), extracts the affected resource addresses and the most relevant log snippet, and returns a suggested fix in plain English. Call this whenever the user asks why a run failed or what went wrong. Read-only; safe to call in readonly and apply modes.",
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
			Name:        "_hcp_tf_workspaces_list",
			Description: "Lists HCP Terraform workspaces for an organization: returns the raw JSON array from `hcptf workspace list` with name, resource count, last-run status, and terraform version per workspace. Read-only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org": map[string]any{"type": "string", "description": "HCP Terraform organization name"},
				},
				"required": []string{"org"},
			},
		},
		{
			Name:        "_hcp_tf_version_audit",
			Description: "Audits Terraform versions across all workspaces in an organization. Groups workspaces by Terraform version, queries OSV.dev (https://api.osv.dev/v1/query) for known CVEs affecting each version, and scores upgrade complexity (Low|Medium|High). Returns version_summary, workspaces_at_risk count, and a plain-English recommendation. Read-only; degrades gracefully when OSV.dev is unreachable.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":         map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"min_version": map[string]any{"type": "string", "description": "Optional baseline (defaults to 1.5.0)"},
				},
				"required": []string{"org"},
			},
		},
		{
			Name:        "_hcp_tf_module_audit",
			Description: "Infers which Terraform Registry modules a workspace uses by examining its resource addresses, then queries the public registry (`hcptf publicregistry module`) for the latest available version of each known module. Pinned versions are not available without access to the workspace's .tf files, so every entry is labeled `pinned_version: unknown` and `status: check_recommended`. Module names not present in the built-in registry map are surfaced separately under `unknown_modules`. Read-only; degrades to `latest_version: unavailable` when an individual registry call fails.",
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
			Name:        "_hcp_tf_provider_audit",
			Description: "Audits Terraform providers in a workspace and partitions known CVEs into what currently affects the pinned version vs. what an upgrade would resolve. Downloads workspace state via `hcptf state download` to extract provider names (falls back to per-resource provider fields). Probes the workspace's most recent plan export for required_providers constraints — exact constraints (e.g. `4.9.0`) populate `pinned_version`, range constraints (`~> 4.45.0`, etc.) leave `pinned_version: \"unknown\"` and the raw constraint is exposed in `version_constraint`. For each hashicorp/* provider, fetches the latest registry version and queries OSV.dev for every known CVE. Each provider entry returns `all_cves` (every known CVE), `currently_affected` (CVEs whose fix postdates the pinned version, or empty when pinned is unknown), and `upgrading_fixes` (CVEs whose fix is reachable by upgrading to the latest version; equals `all_cves` when pinned is unknown). The envelope's `pinned_version_source` is `\"planexport\"` when constraints were discovered, `\"unknown\"` otherwise. Non-hashicorp providers are surfaced under `unknown_providers`. Read-only; degrades to `cve_data_unavailable: true` when OSV is unreachable.",
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
			Name:        "_hcp_tf_stacks_list",
			Description: "Lists HCP Terraform Stacks for an organization: returns the raw JSON array from `hcptf stack list` with id, name, project, status, and deployment counts per stack. Read-only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org": map[string]any{"type": "string", "description": "HCP Terraform organization name"},
				},
				"required": []string{"org"},
			},
		},
		{
			Name:        "_hcp_tf_stack_describe",
			Description: "Describes a single HCP Terraform Stack. Fetches stack metadata, configuration list, and deployment list in parallel and returns { stack_id, name, project, configuration_count, deployments[], deployment_count, health (Healthy|Degraded|Unknown), limitations[] }. Health is derived from deployment statuses; limitations always include no policy as code, no drift detection, and no run tasks. Read-only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":      map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"stack_id": map[string]any{"type": "string", "description": "Stack ID (stk-xxx)"},
				},
				"required": []string{"org", "stack_id"},
			},
		},
		{
			Name:        "_hcp_tf_stack_vs_workspace",
			Description: "Recommends stack vs. workspace for a plain-English use case. Pure reasoning — does not call hcptf. Returns { recommendation: stack|workspace|either, reasoning, use_stack_when[], use_workspace_when[], key_limitations[] }. When policy, drift, or run-task signals appear they override scale signals because those are hard GA blockers for stacks. Call this when a user is deciding whether to model their infrastructure as a Stack or a Workspace.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":      map[string]any{"type": "string", "description": "HCP Terraform organization name (for audit context; not used to query)"},
					"use_case": map[string]any{"type": "string", "description": "Free-text description of what the user is trying to build"},
				},
				"required": []string{"org", "use_case"},
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
			Name:        "_hcp_tf_workspace_create",
			Description: "Creates a new HCP Terraform workspace in an organization, optionally within a named project. Returns { workspace_id, name, org, project, url }. Mutating — only available when --apply is set. The caller must obtain explicit user approval before calling this tool.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":               map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"name":              map[string]any{"type": "string", "description": "Name of the workspace to create"},
					"project":           map[string]any{"type": "string", "description": "Optional project name to place the workspace in. Resolved to a project_id automatically."},
					"project_id":        map[string]any{"type": "string", "description": "Optional project ID (prj-xxx). Overrides project when both are set."},
					"description":       map[string]any{"type": "string", "description": "Optional human description for the workspace"},
					"terraform_version": map[string]any{"type": "string", "description": "Optional Terraform version constraint, e.g. \"~>1.0\""},
				},
				"required": []string{"org", "name"},
			},
		},
		{
			Name:        "_hcp_tf_workspace_populate",
			Description: "Uploads Terraform HCL configuration to a workspace and triggers a run in one step. Writes the config string to a temp directory, creates a configuration version, uploads the files, and creates a run. Returns { run_id, status, workspace, terraform_init, message }. The caller should generate and self-validate HCL with _hcp_tf_config_validate before calling this tool. Mutating — only available when --apply is set.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":       map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace": map[string]any{"type": "string", "description": "Target workspace name (must already exist — use _hcp_tf_workspace_create first if needed)"},
					"config":    map[string]any{"type": "string", "description": "Full Terraform HCL as a single string. The tool writes it to main.tf inside a temp directory and uploads the directory."},
					"message":   map[string]any{"type": "string", "description": "Optional run message (default: \"tfpilot: initial resource provisioning\")"},
				},
				"required": []string{"org", "workspace", "config"},
			},
		},
		{
			Name:        "_hcp_tf_version_upgrade",
			Description: "Upgrades a workspace's Terraform required_version by generating a minimal terraform{} HCL stub (`terraform { required_version = \"~> <target_version>\" }`), uploading it as a new configuration version, and triggering a run. Returns { org, workspace, target_version, run_id, status, message }. Because the uploaded config contains only the terraform block, the resulting plan will propose to destroy any existing resources alongside the version bump — the caller MUST chain into _hcp_tf_plan_analyze on run_id and obtain explicit user approval before _hcp_tf_run_apply. Mutating — only available when --apply is set.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":            map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace":      map[string]any{"type": "string", "description": "Target workspace name"},
					"target_version": map[string]any{"type": "string", "description": "Target Terraform version, e.g. \"1.14.9\""},
				},
				"required": []string{"org", "workspace", "target_version"},
			},
		},
		{
			Name:        "_hcp_tf_batch_upgrade",
			Description: "Builds a prioritized upgrade queue from a comma-separated list of vulnerable workspaces. Returns { org, target_version, mode, total, queue[{workspace, current_version, target_version, cve_count, highest_severity, cve_ids, resource_count, last_run_destructions, risk_flag, priority}] }. Sort key: highest CVE severity, then most resources, then \"prod\" substring. risk_flag is true when resource_count > 50 or last_run_destructions > 0; the REPL auto-pauses for risk_flag workspaces regardless of mode. The tool does not execute upgrades — the REPL drives the per-workspace approval loop, calling _hcp_tf_version_upgrade per entry. Mutating — only available when --apply is set, because every queued workspace will be mutated during the loop.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":            map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspaces":     map[string]any{"type": "string", "description": "Comma-separated list of workspace names to queue for upgrade"},
					"target_version": map[string]any{"type": "string", "description": "Target Terraform version, e.g. \"1.14.9\""},
					"mode":           map[string]any{"type": "string", "description": "Optional: \"interactive\" (default) or \"auto\". Recorded in output; the REPL still pauses for risk_flag workspaces regardless of mode."},
				},
				"required": []string{"org", "workspaces", "target_version"},
			},
		},
		{
			Name:        "_hcp_tf_compliance_report",
			Description: "Aggregates a batch upgrade's results into a CISO-shareable markdown report and writes it to compliance-report-<timestamp>.md. By default writes to the current working directory; pass output_dir to redirect (created if missing, leading ~ expanded). Pure local transformation — pass the JSON array of batch results from a prior batch upgrade run as `results`. Returns { org, generated_at, target_version, summary{total_workspaces,upgraded,skipped,failed,noop,cves_resolved,cve_ids_resolved}, upgraded_workspaces, skipped_workspaces, failed_workspaces, report_markdown, report_path }. Read-only with respect to HCP Terraform; writes to local disk only.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":            map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"results":        map[string]any{"type": "string", "description": "JSON array of batch results: [{workspace, previous_version, new_version, status, risk_score, cves_resolved[], run_id, error_code, duration_ms}]"},
					"target_version": map[string]any{"type": "string", "description": "Optional target version annotated in the report header"},
					"report_format":  map[string]any{"type": "string", "description": "Optional: \"markdown\" (default) or \"text\""},
					"output_dir":     map[string]any{"type": "string", "description": "Optional output directory (e.g. \"~/.tfpilot/reports\"). Created if missing. Defaults to current working directory."},
				},
				"required": []string{"org", "results"},
			},
		},
		{
			Name:        "_hcp_tf_compliance_summary",
			Description: "Builds an org-wide compliance posture snapshot for security/audit review readiness. Internally fans out _hcp_tf_version_audit (Terraform CVEs across all workspaces) and, when include_providers=\"true\", a top-3-by-resource-count _hcp_tf_provider_audit fan-out for provider CVEs (default off; ~30s when on). Computes a severity-weighted compliance_score (0-100, null when CVE data is unavailable), top_cves rolled up across the org, remediation_priority list sorted by severity → resources → prod-name signal, and a plain-English compliance_verdict (✓ healthy / ⚠ warning / ✗ degraded / indeterminate). Read-only. Returns { org, generated_at, compliance_score, compliance_verdict, ready_for_review, total_workspaces, compliant_workspaces, at_risk_workspaces, critical_workspaces, top_cves[], remediation_priority[], cve_data_unavailable, provider_data_partial, provider_audits[], include_providers }.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":               map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"include_providers": map[string]any{"type": "string", "description": "Optional. \"true\" to add a top-3-by-resource provider_audit fan-out (~30s wall time). Defaults to \"false\" (fast path, version-audit only, ~5s)."},
				},
				"required": []string{"org"},
			},
		},
		{
			Name:        "_hcp_tf_upgrade_preview",
			Description: "Previews the safety of upgrading a provider in a workspace. Generates a speculative plan by uploading the local HCL config (with the named provider's version constraint rewritten to `= <target_version>`) as a speculative configuration version, polls for the auto-queued plan-only run, and feeds it through `_hcp_tf_plan_analyze` for risk_level and blast_radius. Cross-references CVEs from `_hcp_tf_provider_audit` to compute `cves_fixed`, fetches GitHub release notes between the pinned and target versions to extract `breaking_changes`, and synthesizes a `recommendation` (go|review|no_go). The speculative run is discarded after analysis. Mutating — only available when --apply is set; the speculative run never applies but a configversion is created. The tool reads HCL from `config_path` (defaults to the current working directory). Honors GITHUB_TOKEN for higher GitHub API rate limits.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"org":            map[string]any{"type": "string", "description": "HCP Terraform organization name"},
					"workspace":      map[string]any{"type": "string", "description": "Workspace name"},
					"provider":       map[string]any{"type": "string", "description": "Provider short name as it appears in `required_providers` (e.g. \"aws\", \"google\", \"azurerm\")"},
					"target_version": map[string]any{"type": "string", "description": "Target provider version (e.g. \"5.91.0\"). If omitted, call _hcp_tf_provider_audit first to discover latest_version."},
					"config_path":    map[string]any{"type": "string", "description": "Optional path to a directory containing the workspace's HCL. Defaults to the current working directory. The tool will rewrite the provider's version constraint and upload as a speculative configversion."},
				},
				"required": []string{"org", "workspace", "provider", "target_version"},
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
