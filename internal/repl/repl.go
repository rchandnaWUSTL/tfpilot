package repl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chzyer/readline"
	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/tfpilot/internal/agent"
	"github.com/rchandnaWUSTL/tfpilot/internal/config"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/tools"
)

var (
	bold         = color.New(color.Bold)
	cyan         = color.New(color.FgCyan)
	green        = color.New(color.FgGreen)
	red          = color.New(color.FgRed)
	yellow       = color.New(color.FgYellow)
	white        = color.New(color.FgWhite)
	dimWhite     = color.New(color.FgWhite, color.Faint)
	boldWhite    = color.New(color.FgWhite, color.Bold)
	tfPurple     = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(99), color.Bold)  // HashiCorp Terraform #7B42BC
	packerBlue   = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(39), color.Bold)  // HashiCorp Packer #02A8EF
	waypointTeal = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(44))              // HashiCorp Waypoint #14C6CB
	vaultYellow  = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(220))             // HashiCorp Vault #FFCF25
	boundaryPink = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(203))             // HashiCorp Boundary #EC585D
)

type REPL struct {
	cfg       *config.Config
	ag        *agent.Agent
	prov      provider.Provider
	org       string
	workspace string

	mu              sync.Mutex
	lastPlanSummary json.RawMessage
	lastRunID       string
	discardedRuns   map[string]bool
	rl              *readline.Instance
}

func New(cfg *config.Config, prov provider.Provider, org, workspace string) *REPL {
	return &REPL{
		cfg:           cfg,
		ag:            agent.New(cfg, prov),
		prov:          prov,
		org:           org,
		workspace:     workspace,
		discardedRuns: map[string]bool{},
	}
}

// openReadline builds a fresh readline instance. readYes closes and reopens
// around the approval prompt so a plain bufio.Scanner read on os.Stdin is not
// racing readline's internal Terminal goroutine for input bytes.
func (r *REPL) openReadline() error {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          cyan.Sprint("hcp-tf> "),
		HistoryFile:     historyPath(),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("readline: %w", err)
	}
	r.rl = rl
	return nil
}

func (r *REPL) closeReadline() {
	if r.rl != nil {
		_ = r.rl.Close()
		r.rl = nil
	}
}

func (r *REPL) Run() error {
	color.NoColor = false
	printBanner(r.cfg)

	if err := r.openReadline(); err != nil {
		return err
	}
	defer r.closeReadline()

	for {
		line, err := r.rl.Readline()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if done := r.handleSlash(line); done {
				break
			}
			continue
		}

		r.ask(line)
	}
	return nil
}

func (r *REPL) handleSlash(cmd string) (exit bool) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/exit", "/quit":
		fmt.Println("Goodbye.")
		return true
	case "/help":
		printHelp()
	case "/reset":
		r.ag.Reset()
		dimWhite.Println("Conversation reset.")
	case "/org":
		if len(parts) > 1 {
			r.org = parts[1]
			green.Printf("org set to %s\n", r.org)
		} else {
			fmt.Printf("org: %s\n", r.org)
		}
	case "/workspace":
		if len(parts) > 1 {
			r.workspace = parts[1]
			green.Printf("workspace set to %s\n", r.workspace)
		} else {
			fmt.Printf("workspace: %s\n", r.workspace)
		}
	case "/mode":
		if r.cfg.Readonly {
			green.Println("mode: readonly (default)")
		} else {
			yellow.Println("mode: read-write")
		}
	case "/analyze":
		r.handleAnalyze(parts[1:])
	case "/diagnose":
		r.handleDiagnose(parts[1:])
	case "/stacks":
		r.handleStacks()
	case "/workspaces":
		r.handleWorkspaces(parts[1:])
	default:
		boundaryPink.Printf("Unknown command: %s\n", parts[0])
		fmt.Println("Type /help for available commands.")
	}
	return false
}

func (r *REPL) ask(userMsg string) {
	ctx := context.Background()

	var sawToolResult atomic.Bool
	var spin *toolSpinner
	ch, err := r.ag.Ask(ctx, userMsg, r.org, r.workspace,
		func(ev agent.ToolCallEvent) {
			spin = startToolSpinner(ev)
		},
		func(name string, result *tools.CallResult) {
			if spin != nil {
				spin.finish(name, result)
				spin = nil
			} else {
				printToolResult(name, result)
			}
			sawToolResult.Store(true)
			r.recordToolResult(name, result)
		},
		r.approveMutation,
	)
	if err != nil {
		boundaryPink.Printf("Error: %v\n", err)
		return
	}

	fmt.Println()
	var buf strings.Builder
	var full strings.Builder
	firstLine := true
	flushLine := func(line string) {
		clean := stripMarkdown(line)
		if clean == "" && firstLine {
			return
		}
		if firstLine && sawToolResult.Load() {
			fmt.Println()
		}
		white.Printf("  %s\n", clean)
		firstLine = false
	}
	for chunk := range ch {
		if chunk.Err != nil {
			if buf.Len() > 0 {
				flushLine(buf.String())
				buf.Reset()
			}
			boundaryPink.Printf("Error: %v\n", chunk.Err)
			return
		}
		if chunk.Done {
			break
		}
		buf.WriteString(chunk.Text)
		full.WriteString(chunk.Text)
		for {
			s := buf.String()
			idx := strings.IndexByte(s, '\n')
			if idx < 0 {
				break
			}
			flushLine(s[:idx])
			buf.Reset()
			buf.WriteString(s[idx+1:])
		}
	}
	if buf.Len() > 0 {
		flushLine(buf.String())
	}
	fmt.Println()

	r.handleGeneratedConfig(ctx, full.String())
}

// recordToolResult captures the last successful plan_summary and run_create
// payloads so the approval gate can warn on destructive plans and trigger a
// matching discard when the user cancels an apply.
func (r *REPL) recordToolResult(name string, result *tools.CallResult) {
	if result == nil || result.Err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch name {
	case "_hcp_tf_plan_summary":
		r.lastPlanSummary = result.Output
	case "_hcp_tf_run_create":
		if id := extractRunID(result.Output); id != "" {
			r.lastRunID = id
		}
	case "_hcp_tf_run_discard":
		if id := result.Args["run_id"]; id != "" {
			r.discardedRuns[id] = true
		}
	}
}

// approveMutation prompts the user before a mutating tool executes. It returns
// true when the user types "yes". Apply operations additionally trigger a
// structured risk assessment via _hcp_tf_plan_analyze; the required
// confirmation scales with the returned risk_level (Low/Medium → single yes,
// High → yes twice, Critical → type the workspace name). On cancellation of
// an apply, any previously-created run is discarded synchronously so it does
// not remain pending in HCP Terraform.
func (r *REPL) approveMutation(name string, args map[string]string) bool {
	// Already-discarded runs get an automatic pass on follow-up discard calls
	// so the agent's "call discard on cancel" rule doesn't double-prompt.
	if name == "_hcp_tf_run_discard" {
		r.mu.Lock()
		discarded := r.discardedRuns[args["run_id"]]
		r.mu.Unlock()
		if discarded {
			return true
		}
	}

	if name == "_hcp_tf_run_apply" {
		return r.applyGate(args)
	}

	action := describeAction(name, args)
	fmt.Println()
	vaultYellow.Printf("  ⚠ This will %s. Type 'yes' to confirm or anything else to cancel.\n", action)

	if !r.readYes() {
		r.onMutationCancelled(name, args)
		return false
	}
	return true
}

// applyGate runs _hcp_tf_plan_analyze, renders the risk assessment, and
// prompts for confirmation with a strength scaled to the returned risk level.
// When analyze fails, it falls back to the legacy destroys-based gate so the
// REPL is still safe against network or permission errors on the analyze call.
func (r *REPL) applyGate(args map[string]string) bool {
	runID := args["run_id"]

	analyzeArgs := map[string]string{
		"org":       r.org,
		"workspace": r.workspace,
		"run_id":    runID,
	}
	actx, acancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer acancel()
	analyze := tools.Call(actx, "_hcp_tf_plan_analyze", analyzeArgs, r.cfg.TimeoutSeconds)

	fmt.Println()
	if analyze.Err != nil {
		boundaryPink.Printf("  ✗ _hcp_tf_plan_analyze failed: %s\n", analyze.Err.Message)
		return r.applyGateLegacy(args)
	}

	assessment := parseAssessment(analyze.Output)
	renderAssessment(assessment)

	action := describeAction("_hcp_tf_run_apply", args)
	switch {
	case assessment.policyFailed || assessment.riskLevel == "Critical":
		return r.criticalConfirm(args, action, assessment)
	case assessment.riskLevel == "High":
		return r.highConfirm(args, action)
	default:
		vaultYellow.Printf("  ⚠ This will %s. Type 'yes' to confirm or anything else to cancel.\n", action)
		if !r.readYes() {
			r.onMutationCancelled("_hcp_tf_run_apply", args)
			return false
		}
		return true
	}
}

// applyGateLegacy preserves the pre-v0.7 destroys-based gate behavior so that
// a transient analyze failure does not make apply either impossibly strict or
// silently permissive.
func (r *REPL) applyGateLegacy(args map[string]string) bool {
	action := describeAction("_hcp_tf_run_apply", args)
	vaultYellow.Printf("  ⚠ This will %s. Type 'yes' to confirm or anything else to cancel.\n", action)
	if destroys := r.destroysFromLastPlan(); destroys > 0 {
		boundaryPink.Printf("  ✗ This plan will destroy %d resource(s). Type 'yes' again to confirm destruction.\n", destroys)
	}
	if !r.readYes() {
		r.onMutationCancelled("_hcp_tf_run_apply", args)
		return false
	}
	if r.destroysFromLastPlan() > 0 {
		boundaryPink.Println("  ✗ Confirm destruction.")
		if !r.readYes() {
			r.onMutationCancelled("_hcp_tf_run_apply", args)
			return false
		}
	}
	return true
}

// criticalConfirm requires the user to type the exact workspace name — a
// deliberate mismatch with the usual "yes" keyword so the operator has to
// acknowledge which workspace they are mutating.
func (r *REPL) criticalConfirm(args map[string]string, action string, a assessmentResult) bool {
	boundaryPink.Add(color.Bold).Printf("  ✗ CRITICAL risk. This will %s.\n", action)
	if len(a.failedPolicies) > 0 {
		boundaryPink.Printf("  ✗ Failed policies: %s\n", strings.Join(a.failedPolicies, ", "))
	}
	boundaryPink.Printf("  ✗ Type the workspace name '%s' to confirm, or anything else to cancel.\n", r.workspace)
	if r.readLine() != r.workspace {
		r.onMutationCancelled("_hcp_tf_run_apply", args)
		return false
	}
	return true
}

// highConfirm requires two independent yes confirmations so the operator
// cannot fat-finger past a High-risk apply.
func (r *REPL) highConfirm(args map[string]string, action string) bool {
	vaultYellow.Printf("  ⚠ HIGH risk. This will %s. Type 'yes' to confirm or anything else to cancel.\n", action)
	if !r.readYes() {
		r.onMutationCancelled("_hcp_tf_run_apply", args)
		return false
	}
	boundaryPink.Println("  ✗ Confirm again. Type 'yes' once more to proceed.")
	if !r.readYes() {
		r.onMutationCancelled("_hcp_tf_run_apply", args)
		return false
	}
	return true
}

// readYes suspends readline while prompting, so input goes to this scanner
// instead of being swallowed by readline's background Terminal goroutine.
// chzyer/readline v1.5.1 has no Pause/Resume, so we close the instance and
// reopen it once the user has answered.
func (r *REPL) readYes() bool {
	return r.readLine() == "yes"
}

// readLine captures a single line of input at an approval prompt. Like
// readYes, it closes readline for the duration of the read so input is not
// lost to the background Terminal goroutine.
func (r *REPL) readLine() string {
	r.closeReadline()
	defer func() {
		if err := r.openReadline(); err != nil {
			boundaryPink.Printf("  ✗ Failed to reopen readline: %v\n", err)
		}
	}()

	fmt.Print("  > ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return ""
	}
	return strings.TrimSpace(scanner.Text())
}

// handleAnalyze implements /analyze <run-id> by invoking _hcp_tf_plan_analyze
// with the current org/workspace and rendering the returned assessment. A
// missing or malformed run-id produces a friendly error instead of panicking.
func (r *REPL) handleAnalyze(args []string) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		boundaryPink.Println("Usage: /analyze <run-id>")
		return
	}
	runID := strings.TrimSpace(args[0])
	if !strings.HasPrefix(runID, "run-") {
		boundaryPink.Printf("Invalid run ID %q — expected a run-xxx identifier.\n", runID)
		return
	}
	if r.org == "" || r.workspace == "" {
		boundaryPink.Println("Set /org and /workspace before running /analyze.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	result := tools.Call(ctx, "_hcp_tf_plan_analyze", map[string]string{
		"org":       r.org,
		"workspace": r.workspace,
		"run_id":    runID,
	}, r.cfg.TimeoutSeconds)

	fmt.Println()
	if result.Err != nil {
		boundaryPink.Printf("  ✗ _hcp_tf_plan_analyze: %s\n", result.Err.Message)
		return
	}
	renderAssessment(parseAssessment(result.Output))
}

// handleWorkspaces implements /workspaces by calling _hcp_tf_workspaces_list
// for the pinned org and rendering a one-line-per-workspace summary. When a
// filter argument is supplied, only workspaces whose name contains the filter
// (case-insensitive) are shown.
func (r *REPL) handleWorkspaces(args []string) {
	if r.org == "" {
		boundaryPink.Println("Set an org first with /org <name>")
		return
	}

	filter := strings.Join(args, " ")
	filterLower := strings.ToLower(filter)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	result := tools.Call(ctx, "_hcp_tf_workspaces_list", map[string]string{"org": r.org}, r.cfg.TimeoutSeconds)

	fmt.Println()
	if result.Err != nil {
		boundaryPink.Printf("  ✗ _hcp_tf_workspaces_list: %s\n", result.Err.Message)
		return
	}

	var workspaces []map[string]any
	if len(result.Output) > 0 {
		_ = json.Unmarshal(result.Output, &workspaces)
	}

	if filter != "" {
		filtered := workspaces[:0]
		for _, ws := range workspaces {
			name := stringField(ws, "name", "Name")
			if strings.Contains(strings.ToLower(name), filterLower) {
				filtered = append(filtered, ws)
			}
		}
		workspaces = filtered
	}

	if len(workspaces) == 0 {
		if filter != "" {
			white.Printf("  No workspaces matching '%s' in %s.\n", filter, r.org)
		} else {
			white.Printf("  No workspaces found in %s.\n", r.org)
		}
		return
	}

	if filter != "" {
		waypointTeal.Printf("  Workspaces in %s matching '%s':\n", r.org, filter)
	} else {
		waypointTeal.Printf("  Workspaces in %s:\n", r.org)
	}
	fmt.Println()
	for _, ws := range workspaces {
		name := stringField(ws, "name", "Name")
		count := intField(ws, "resource_count", "ResourceCount", "resource-count")
		status := stringField(ws, "current_run_status", "CurrentRunStatus", "current-run-status")
		if status == "" {
			status = "no runs"
		}
		fmt.Print("  • ")
		boldWhite.Print(name)
		fmt.Printf("    %d resources    ", count)
		dimWhite.Println(status)
	}
}

// handleStacks implements /stacks by calling _hcp_tf_stacks_list for the
// pinned org and rendering a one-line-per-stack summary. An empty result is
// rendered as an explanatory hint about when Stacks are the right tool, with
// a link to the docs.
func (r *REPL) handleStacks() {
	if r.org == "" {
		boundaryPink.Println("Set /org before running /stacks.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	result := tools.Call(ctx, "_hcp_tf_stacks_list", map[string]string{"org": r.org}, r.cfg.TimeoutSeconds)

	fmt.Println()
	if result.Err != nil {
		boundaryPink.Printf("  ✗ _hcp_tf_stacks_list: %s\n", result.Err.Message)
		return
	}

	var stacks []map[string]any
	if len(result.Output) > 0 {
		_ = json.Unmarshal(result.Output, &stacks)
	}

	if len(stacks) == 0 {
		white.Printf("  No stacks found in %s. Stacks are used for repeated infrastructure across environments or regions.\n", r.org)
		dimWhite.Println("  Learn more: https://developer.hashicorp.com/terraform/cloud-docs/stacks")
		return
	}

	waypointTeal.Printf("  Stacks in %s:\n", r.org)
	fmt.Println()
	for _, s := range stacks {
		name := stringField(s, "name", "Name")
		project := stringField(s, "project", "Project", "project_name", "ProjectName")
		count := intField(s, "deployment_count", "DeploymentCount", "deployments", "Deployments")
		health := stringField(s, "health", "Health")
		if health == "" {
			health = "Unknown"
		}
		fmt.Print("  • ")
		boldWhite.Print(name)
		if project != "" {
			dimWhite.Printf(" (%s)", project)
		}
		fmt.Printf("    %d deployments    ", count)
		switch health {
		case "Healthy":
			waypointTeal.Println(health)
		case "Degraded":
			boundaryPink.Println(health)
		default:
			dimWhite.Println(health)
		}
	}
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if n, ok := toInt(m[k]); ok {
			return n
		}
		if arr, ok := m[k].([]any); ok {
			return len(arr)
		}
	}
	return 0
}

func (r *REPL) handleDiagnose(args []string) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		boundaryPink.Println("Usage: /diagnose <run-id>")
		return
	}
	runID := strings.TrimSpace(args[0])
	if !strings.HasPrefix(runID, "run-") {
		boundaryPink.Printf("Invalid run ID %q — expected a run-xxx identifier.\n", runID)
		return
	}
	if r.org == "" || r.workspace == "" {
		boundaryPink.Println("Set /org and /workspace before running /diagnose.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	result := tools.Call(ctx, "_hcp_tf_run_diagnose", map[string]string{
		"org":       r.org,
		"workspace": r.workspace,
		"run_id":    runID,
	}, r.cfg.TimeoutSeconds)

	fmt.Println()
	if result.Err != nil {
		boundaryPink.Printf("  ✗ _hcp_tf_run_diagnose: %s\n", result.Err.Message)
		return
	}
	renderDiagnosis(parseDiagnosis(result.Output))
}

// diagnosisResult is the decoded subset of a _hcp_tf_run_diagnose payload the
// REPL renders. Fields are optional — missing ones collapse to empty values so
// the renderer can degrade gracefully on partial responses.
type diagnosisResult struct {
	runID       string
	status      string
	category    string
	summary     string
	detail      string
	resources   []string
	logSnippet  string
	suggestFix  string
}

func parseDiagnosis(raw json.RawMessage) diagnosisResult {
	d := diagnosisResult{}
	if len(raw) == 0 {
		return d
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return d
	}
	if s, ok := m["run_id"].(string); ok {
		d.runID = s
	}
	if s, ok := m["status"].(string); ok {
		d.status = s
	}
	if s, ok := m["error_category"].(string); ok {
		d.category = s
	}
	if s, ok := m["error_summary"].(string); ok {
		d.summary = s
	}
	if s, ok := m["error_detail"].(string); ok {
		d.detail = s
	}
	if s, ok := m["log_snippet"].(string); ok {
		d.logSnippet = s
	}
	if s, ok := m["suggested_fix"].(string); ok {
		d.suggestFix = s
	}
	if arr, ok := m["affected_resources"].([]any); ok {
		for _, r := range arr {
			if s, ok := r.(string); ok {
				d.resources = append(d.resources, s)
			}
		}
	}
	return d
}

// renderDiagnosis prints the categorized failure using the HashiCorp palette.
// Header in boundaryPink, body text in white, log snippet in dimWhite, fix in
// vaultYellow. Empty sections are skipped rather than rendered as blanks.
func renderDiagnosis(d diagnosisResult) {
	category := strings.ToUpper(d.category)
	if category == "" {
		category = "UNKNOWN"
	}
	fmt.Print("  ")
	boundaryPink.Add(color.Bold).Printf("Error Category: %s\n", category)

	if d.summary != "" || d.detail != "" {
		fmt.Println()
		white.Println("  What went wrong:")
		if d.summary != "" {
			white.Printf("    %s\n", d.summary)
		}
		if d.detail != "" {
			dimWhite.Printf("    %s\n", d.detail)
		}
	}

	if len(d.resources) > 0 {
		fmt.Println()
		white.Println("  Affected resources:")
		for _, r := range d.resources {
			white.Printf("    • %s\n", r)
		}
	}

	if d.logSnippet != "" {
		fmt.Println()
		white.Println("  Log snippet:")
		for _, line := range strings.Split(d.logSnippet, "\n") {
			dimWhite.Printf("    %s\n", line)
		}
	}

	if d.suggestFix != "" {
		fmt.Println()
		white.Println("  Suggested fix:")
		vaultYellow.Printf("    %s\n", d.suggestFix)
	}
}

// assessmentResult is the decoded subset of a _hcp_tf_plan_analyze payload the
// REPL renders and branches on. The raw result is also preserved so /analyze
// can print the full structure without re-running the tool.
type assessmentResult struct {
	runID          string
	riskLevel      string
	riskFactors    []assessmentFactor
	blastRadius    assessmentBlastRadius
	policyPresent  bool
	policyTotal    int
	policyPassed   int
	policyFailed   bool
	failedPolicies []string
	recommendation string
	reason         string
}

type assessmentFactor struct {
	factor    string
	severity  string
	resources []string
}

type assessmentBlastRadius struct {
	total        int
	additions    int
	changes      int
	destructions int
	highestRisk  []string
}

// parseAssessment decodes a _hcp_tf_plan_analyze payload into the subset the
// REPL renders. Missing fields collapse to zero values rather than erroring so
// the gate can still fall back to legacy behavior on a partial response.
func parseAssessment(raw json.RawMessage) assessmentResult {
	a := assessmentResult{}
	if len(raw) == 0 {
		return a
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return a
	}
	if s, ok := m["run_id"].(string); ok {
		a.runID = s
	}
	if s, ok := m["risk_level"].(string); ok {
		a.riskLevel = s
	}
	if s, ok := m["recommendation"].(string); ok {
		a.recommendation = s
	}
	if s, ok := m["recommendation_reason"].(string); ok {
		a.reason = s
	}
	if arr, ok := m["risk_factors"].([]any); ok {
		for _, item := range arr {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			f := assessmentFactor{}
			if s, ok := obj["factor"].(string); ok {
				f.factor = s
			}
			if s, ok := obj["severity"].(string); ok {
				f.severity = s
			}
			if rs, ok := obj["resources"].([]any); ok {
				for _, r := range rs {
					if s, ok := r.(string); ok {
						f.resources = append(f.resources, s)
					}
				}
			}
			a.riskFactors = append(a.riskFactors, f)
		}
	}
	if br, ok := m["blast_radius"].(map[string]any); ok {
		a.blastRadius.total, _ = toInt(br["total_resources_affected"])
		a.blastRadius.additions, _ = toInt(br["additions"])
		a.blastRadius.changes, _ = toInt(br["changes"])
		a.blastRadius.destructions, _ = toInt(br["destructions"])
		if hr, ok := br["highest_risk_resources"].([]any); ok {
			for _, r := range hr {
				if s, ok := r.(string); ok {
					a.blastRadius.highestRisk = append(a.blastRadius.highestRisk, s)
				}
			}
		}
	}
	if pc, ok := m["policy_checks"].(map[string]any); ok {
		a.policyPresent = true
		a.policyTotal, _ = toInt(pc["total"])
		a.policyPassed, _ = toInt(pc["passed"])
		if failed, _ := toInt(pc["failed"]); failed > 0 {
			a.policyFailed = true
		}
		if fp, ok := pc["failed_policies"].([]any); ok {
			for _, r := range fp {
				if s, ok := r.(string); ok {
					a.failedPolicies = append(a.failedPolicies, s)
				}
			}
		}
	}
	return a
}

// renderAssessment prints the risk level, factors, blast radius, policy
// results, and recommendation using the HashiCorp palette. Risk level color:
// Low=waypointTeal, Medium=vaultYellow, High=boundaryPink, Critical=bold pink.
func renderAssessment(a assessmentResult) {
	risk := a.riskLevel
	if risk == "" {
		risk = "Unknown"
	}
	riskUpper := strings.ToUpper(risk)

	var rc *color.Color
	switch risk {
	case "Critical":
		rc = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(203), color.Bold)
	case "High":
		rc = boundaryPink
	case "Medium":
		rc = vaultYellow
	case "Low":
		rc = waypointTeal
	default:
		rc = dimWhite
	}
	fmt.Print("  ")
	rc.Printf("Risk Level: %s\n", riskUpper)

	if len(a.riskFactors) > 0 {
		fmt.Println()
		white.Println("  Risk Factors:")
		for _, f := range a.riskFactors {
			resList := ""
			if len(f.resources) > 0 {
				resList = " — " + strings.Join(f.resources, ", ")
			}
			white.Printf("    • %s (%s)%s\n", f.factor, f.severity, resList)
		}
	}

	fmt.Println()
	white.Println("  Blast Radius:")
	white.Printf("    %d resources affected: %d additions, %d changes, %d destructions\n",
		a.blastRadius.total, a.blastRadius.additions, a.blastRadius.changes, a.blastRadius.destructions)
	if len(a.blastRadius.highestRisk) > 0 {
		white.Printf("    Highest risk: %s\n", strings.Join(a.blastRadius.highestRisk, ", "))
	}

	if a.policyPresent {
		fmt.Println()
		white.Printf("  Policy Checks: %d passed / %d failed\n", a.policyPassed, a.policyTotal-a.policyPassed)
		for _, name := range a.failedPolicies {
			boundaryPink.Printf("    ✗ %s\n", name)
		}
	}

	if a.recommendation != "" || a.reason != "" {
		fmt.Println()
		label := a.recommendation
		if label == "" {
			label = "unknown"
		}
		white.Printf("  Recommendation: %s", label)
		if a.reason != "" {
			white.Printf(" — %s", a.reason)
		}
		fmt.Println()
	}
}

// onMutationCancelled prints the cancellation marker and, if an apply is being
// cancelled after a run was created, synchronously discards that run.
func (r *REPL) onMutationCancelled(name string, args map[string]string) {
	r.mu.Lock()
	runID := r.lastRunID
	alreadyDiscarded := runID != "" && r.discardedRuns[runID]
	r.mu.Unlock()

	if name == "_hcp_tf_run_apply" && runID != "" && !alreadyDiscarded {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
		defer cancel()
		discard := tools.Call(ctx, "_hcp_tf_run_discard", map[string]string{
			"run_id":  runID,
			"comment": "cancelled at approval gate",
		}, r.cfg.TimeoutSeconds)
		printToolResult("_hcp_tf_run_discard", discard)
		if discard.Err == nil {
			r.mu.Lock()
			r.discardedRuns[runID] = true
			r.mu.Unlock()
		}
	}
	boundaryPink.Println("  Cancelled.")
}

func (r *REPL) destroysFromLastPlan() int {
	r.mu.Lock()
	raw := r.lastPlanSummary
	r.mu.Unlock()
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	for _, key := range []string{"destroy", "destroys", "resource_destructions", "ResourceDestructions"} {
		if n, ok := toInt(m[key]); ok {
			return n
		}
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		var x int
		if _, err := fmt.Sscanf(n, "%d", &x); err == nil {
			return x, true
		}
	}
	return 0, false
}

func extractRunID(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range []string{"ID", "id", "run_id", "RunID"} {
		if s, ok := m[key].(string); ok && strings.HasPrefix(s, "run-") {
			return s
		}
	}
	return ""
}

func describeAction(name string, args map[string]string) string {
	switch name {
	case "_hcp_tf_run_create":
		if ws := args["workspace"]; ws != "" {
			return fmt.Sprintf("create a new run in %s", ws)
		}
		return "create a new run"
	case "_hcp_tf_run_apply":
		if ws := args["workspace"]; ws != "" {
			return fmt.Sprintf("apply the pending run in %s", ws)
		}
		return "apply the pending run"
	case "_hcp_tf_run_discard":
		return "discard the pending run"
	case "_hcp_tf_workspace_create":
		if n := args["name"]; n != "" {
			if p := args["project"]; p != "" {
				return fmt.Sprintf("create workspace %s in project %s", n, p)
			}
			return fmt.Sprintf("create workspace %s", n)
		}
		return "create a new workspace"
	case "_hcp_tf_workspace_populate":
		if ws := args["workspace"]; ws != "" {
			return fmt.Sprintf("upload config and trigger a run in %s", ws)
		}
		return "upload config and trigger a run"
	}
	return "perform a mutation"
}

// reHCLBlock matches fenced HCL/Terraform code blocks in the agent's response
// and captures the body.
var reHCLBlock = regexp.MustCompile("(?s)```(?:hcl|terraform|tf)\\s*\n(.*?)```")

// reFilenameHint captures `# filename: path.tf` hints in a code block so the
// agent can name generated files.
var reFilenameHint = regexp.MustCompile(`(?m)^\s*#\s*filename:\s*([^\s]+)\s*$`)

// handleGeneratedConfig scans the final agent response for HCL code blocks,
// writes them to the current working directory (prompting before overwriting
// existing files), and auto-runs _hcp_tf_config_validate so the user sees the
// validation result inline.
func (r *REPL) handleGeneratedConfig(ctx context.Context, response string) {
	blocks := reHCLBlock.FindAllStringSubmatch(response, -1)
	if len(blocks) == 0 {
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		boundaryPink.Printf("  ✗ Cannot resolve working directory: %v\n", err)
		return
	}

	wrote := false
	for i, m := range blocks {
		body := m[1]
		filename := "suggested_config.tf"
		if hit := reFilenameHint.FindStringSubmatch(body); hit != nil {
			filename = filepath.Base(hit[1])
			body = reFilenameHint.ReplaceAllString(body, "")
		}
		if len(blocks) > 1 && filename == "suggested_config.tf" {
			filename = fmt.Sprintf("suggested_config_%d.tf", i+1)
		}
		path := filepath.Join(cwd, filename)
		if _, err := os.Stat(path); err == nil {
			vaultYellow.Printf("  ⚠ %s already exists. Overwrite? Type 'yes' to confirm or anything else to skip.\n", filename)
			if !r.readYes() {
				boundaryPink.Printf("  Skipped %s\n", filename)
				continue
			}
		}
		content := strings.TrimSpace(body) + "\n"
		if werr := os.WriteFile(path, []byte(content), 0644); werr != nil {
			boundaryPink.Printf("  ✗ Failed to write %s: %v\n", filename, werr)
			continue
		}
		waypointTeal.Printf("  ✓ Written to ./%s\n", filename)
		wrote = true
	}

	if !wrote {
		return
	}

	vctx, vcancel := context.WithTimeout(ctx, time.Duration(r.cfg.TimeoutSeconds*6)*time.Second)
	defer vcancel()
	result := tools.Call(vctx, "_hcp_tf_config_validate", map[string]string{"config_path": cwd}, r.cfg.TimeoutSeconds*6)
	printToolResult("_hcp_tf_config_validate", result)

	r.offerDirectApply(ctx, result, blocks)
}

// offerDirectApply prompts the user to upload the just-generated config to the
// current workspace and trigger a run, when all preconditions hold: validation
// succeeded, the REPL is in --apply mode, and both org + workspace were bound
// at startup. The call flows through the standard mutation approval gate.
func (r *REPL) offerDirectApply(ctx context.Context, validateResult *tools.CallResult, blocks [][]string) {
	if r.cfg.Readonly || r.org == "" || r.workspace == "" {
		return
	}
	if validateResult == nil || validateResult.Err != nil {
		return
	}
	var parsed struct {
		Valid *bool `json:"valid"`
	}
	if len(validateResult.Output) > 0 {
		if err := json.Unmarshal(validateResult.Output, &parsed); err == nil && parsed.Valid != nil && !*parsed.Valid {
			return
		}
	}

	var combined strings.Builder
	for i, m := range blocks {
		body := reFilenameHint.ReplaceAllString(m[1], "")
		combined.WriteString(strings.TrimSpace(body))
		if i < len(blocks)-1 {
			combined.WriteString("\n\n")
		}
	}
	if combined.Len() == 0 {
		return
	}

	fmt.Println()
	vaultYellow.Printf("  ⚠ Apply this config directly to %s? Type 'yes' to upload and trigger a run, or anything else to keep it local only.\n", r.workspace)
	if !r.readYes() {
		return
	}

	args := map[string]string{
		"org":       r.org,
		"workspace": r.workspace,
		"config":    combined.String(),
	}
	if !r.approveMutation("_hcp_tf_workspace_populate", args) {
		return
	}

	spin := startToolSpinner(agent.ToolCallEvent{Name: "_hcp_tf_workspace_populate", Args: map[string]string{"org": r.org, "workspace": r.workspace}})
	pctx, pcancel := context.WithTimeout(ctx, time.Duration(r.cfg.TimeoutSeconds*6)*time.Second)
	defer pcancel()
	popResult := tools.Call(pctx, "_hcp_tf_workspace_populate", args, r.cfg.TimeoutSeconds*6)
	spin.finish("_hcp_tf_workspace_populate", popResult)
}

var spinnerFrames = []string{"|", "/", "-", "\\"}

type toolSpinner struct {
	stop chan struct{}
	done chan struct{}
	name string
	args string
}

func startToolSpinner(ev agent.ToolCallEvent) *toolSpinner {
	argParts := make([]string, 0, len(ev.Args))
	for k, v := range ev.Args {
		argParts = append(argParts, fmt.Sprintf("%s=%s", k, v))
	}
	s := &toolSpinner{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		name: ev.Name,
		args: strings.Join(argParts, " "),
	}
	s.render(spinnerFrames[0])
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				i = (i + 1) % len(spinnerFrames)
				s.render(spinnerFrames[i])
			}
		}
	}()
	return s
}

func (s *toolSpinner) render(frame string) {
	fmt.Print("\r\033[2K  ")
	tfPurple.Print(frame)
	fmt.Print(" ")
	tfPurple.Print(s.name)
	if s.args != "" {
		dimWhite.Printf("  %s", s.args)
	}
}

func (s *toolSpinner) finish(name string, result *tools.CallResult) {
	close(s.stop)
	<-s.done
	fmt.Print("\r\033[2K")
	printToolResult(name, result)
}

func printToolResult(name string, result *tools.CallResult) {
	duration := result.Duration.Round(time.Millisecond)
	if result.Err != nil {
		boundaryPink.Printf("  ✗ %s (%s): %s\n", name, duration, result.Err.Message)
		return
	}

	preview := truncateJSON(result.Output, 120)
	waypointTeal.Printf("  ✓ %s", name)
	dimWhite.Printf(" (%s)  %s\n", duration, preview)
}

func truncateJSON(raw json.RawMessage, maxLen int) string {
	s := strings.ReplaceAll(string(raw), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

var (
	reMdHeader     = regexp.MustCompile(`^\s*#+\s*`)
	reMdBullet     = regexp.MustCompile(`^\s*[-*+]\s+`)
	reMdBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reMdItalic     = regexp.MustCompile(`(^|[^*])\*([^*\s][^*]*[^*\s]|[^*\s])\*([^*]|$)`)
	reMdCode       = regexp.MustCompile("`([^`]+)`")
	reMdTableSep   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$`)
	reMdTableRow   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	reMdBlockquote = regexp.MustCompile(`^\s*>\s*`)
	reMdWhitespace = regexp.MustCompile(`[ \t]{2,}`)
)

// stripMarkdown removes markdown syntax from a single line, returning
// plain prose. Returns "" for purely structural lines (e.g. table separators)
// so callers can drop them.
func stripMarkdown(line string) string {
	s := line
	if reMdTableSep.MatchString(s) {
		return ""
	}
	s = reMdHeader.ReplaceAllString(s, "")
	s = reMdBlockquote.ReplaceAllString(s, "")
	s = reMdBullet.ReplaceAllString(s, "")
	s = reMdBold.ReplaceAllString(s, "$1")
	s = reMdItalic.ReplaceAllString(s, "$1$2$3")
	s = reMdCode.ReplaceAllString(s, "$1")
	if reMdTableRow.MatchString(s) {
		cells := strings.Split(strings.Trim(s, " \t|"), "|")
		for i, c := range cells {
			cells[i] = strings.TrimSpace(c)
		}
		s = strings.Join(cells, "  ")
	}
	s = reMdWhitespace.ReplaceAllString(s, " ")
	return strings.TrimRight(s, " \t")
}

func printBanner(cfg *config.Config) {
	tfRows := []string{
		"  ████████╗███████╗██████╗ ██╗██╗      ██████╗ ████████╗",
		"  ╚══██╔══╝██╔════╝██╔══██╗██║██║     ██╔═══██╗╚══██╔══╝",
		"     ██║   █████╗  ██████╔╝██║██║     ██║   ██║   ██║   ",
		"     ██║   ██╔══╝  ██╔═══╝ ██║██║     ██║   ██║   ██║   ",
		"     ██║   ██║     ██║     ██║███████╗╚██████╔╝   ██║   ",
		"     ╚═╝   ╚═╝     ╚═╝     ╚═╝╚══════╝ ╚═════╝    ╚═╝   ",
	}

	fmt.Println()
	for i := range tfRows {
		tfPurple.Println(tfRows[i])
	}
	fmt.Println()
	white.Println("  AI-powered development for infrastructure-as-code")
	dimWhite.Println("  v0.1.0 • Type /help for commands")
	fmt.Println()

	sepWidth := utf8.RuneCountInString(tfRows[0])
	dimWhite.Println(strings.Repeat("-", sepWidth))
	fmt.Println()
	mode := "readonly"
	if !cfg.Readonly {
		mode = "apply"
	}
	dimWhite.Printf("  model: %s  |  mode: %s  |  type /help for commands\n", cfg.Model, mode)
	fmt.Println()
}

func printHelp() {
	bold.Println("Commands:")
	fmt.Println("  /org <name>        Set default org")
	fmt.Println("  /workspace <name>  Set default workspace")
	fmt.Println("  /mode              Show current mode")
	fmt.Println("  /analyze <run-id>  Risk assessment for a specific run")
	fmt.Println("  /diagnose <run-id> Categorize a failed run and suggest a fix")
	fmt.Println("  /workspaces [filter]   List workspaces in the pinned org (optional name filter)")
	fmt.Println("  /stacks            List Terraform Stacks in the pinned org")
	fmt.Println("  /reset             Clear conversation history")
	fmt.Println("  /help              Show this help")
	fmt.Println("  /exit              Exit")
	fmt.Println()
	bold.Println("Examples:")
	fmt.Println("  Is it safe to apply my latest prod changes to staging?")
	fmt.Println("  Describe the prod-us-east-1 workspace")
	fmt.Println("  Any of my workspaces drifted this week?")
	fmt.Println()
}

func historyPath() string {
	home, _ := os.UserHomeDir()
	dir := home + "/.tfpilot"
	_ = os.MkdirAll(dir, 0700)
	return dir + "/history"
}
