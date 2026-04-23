package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	var wg sync.WaitGroup
	wg.Add(3)
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
	wg.Wait()
	rRun := <-chRun
	rPlanLogs := <-chPlanLogs
	rApplyLogs := <-chApplyLogs

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
	{"auth", []string{"accessdenied", "unauthorized", "not authorized", " 403 ", "invalidclienttokenid", "signaturedoesnotmatch", "invalid credentials"}},
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
	if name == "_hcp_tf_stacks_list" {
		return stacksListCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_stack_describe" {
		return stackDescribeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_stack_vs_workspace" {
		return stackVsWorkspaceCall(ctx, args, timeoutSec)
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
