package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rchandnaWUSTL/tfpilot/internal/tools"
)

// batchResult is one row in lastBatchResults — the per-workspace outcome of
// a batch upgrade walk. The compliance report consumes a JSON array of these.
type batchResult struct {
	Workspace       string   `json:"workspace"`
	PreviousVersion string   `json:"previous_version"`
	NewVersion      string   `json:"new_version"`
	Status          string   `json:"status"` // applied | noop | skipped | failed
	RiskScore       string   `json:"risk_score"`
	CVEsResolved    []string `json:"cves_resolved"`
	RunID           string   `json:"run_id"`
	ErrorCode       string   `json:"error_code,omitempty"`
	DurationMs      int64    `json:"duration_ms"`
}

type batchQueueEntry struct {
	Workspace       string   `json:"workspace"`
	CurrentVersion  string   `json:"current_version"`
	TargetVersion   string   `json:"target_version"`
	CVECount        int      `json:"cve_count"`
	HighestSeverity string   `json:"highest_severity"`
	CVEIDs          []string `json:"cve_ids"`
	ResourceCount   int      `json:"resource_count"`
	LastDestroys    int      `json:"last_run_destructions"`
	RiskFlag        bool     `json:"risk_flag"`
	Priority        int      `json:"priority"`
}

type batchQueuePayload struct {
	Org           string            `json:"org"`
	TargetVersion string            `json:"target_version"`
	Mode          string            `json:"mode"`
	Total         int               `json:"total"`
	Queue         []batchQueueEntry `json:"queue"`
}

type batchDecision int

const (
	batchApprove batchDecision = iota
	batchApproveAll
	batchSkip
	batchStop
)

// maybeRunBatchUpgrade is called after every agent turn. If the agent issued
// _hcp_tf_batch_upgrade during the turn, the result is staged in
// r.pendingBatch — we drain it here and drive the loop.
func (r *REPL) maybeRunBatchUpgrade(ctx context.Context) {
	r.mu.Lock()
	raw := r.pendingBatch
	r.pendingBatch = nil
	r.mu.Unlock()
	if len(raw) == 0 {
		return
	}
	var payload batchQueuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		boundaryPink.Printf("  ✗ batch_upgrade: could not parse queue: %v\n", err)
		return
	}
	if len(payload.Queue) == 0 {
		dimWhite.Println("  No vulnerable workspaces to remediate.")
		return
	}
	if r.cfg.Readonly {
		boundaryPink.Println("  ✗ Batch upgrade requires --apply mode.")
		return
	}

	results := r.handleBatchUpgrade(ctx, payload)
	r.mu.Lock()
	r.lastBatchResults = results
	r.mu.Unlock()
	r.autoGenerateComplianceReport(ctx, payload, results)
}

func (r *REPL) handleBatchUpgrade(ctx context.Context, payload batchQueuePayload) []batchResult {
	r.batchActive = true
	r.batchAutoApprove = false
	defer func() {
		r.batchActive = false
		r.batchAutoApprove = false
	}()

	results := make([]batchResult, 0, len(payload.Queue))

	fmt.Println()
	tfPurple.Printf("  Batch upgrade — %d workspaces queued for Terraform %s\n", payload.Total, payload.TargetVersion)
	dimWhite.Println("  Approval options: yes, yes to all, skip, stop")

	for i, entry := range payload.Queue {
		decision := r.approveBatchWorkspace(i+1, payload.Total, entry)
		switch decision {
		case batchApproveAll:
			r.batchAutoApprove = true
		case batchSkip:
			results = append(results, batchResult{
				Workspace:       entry.Workspace,
				PreviousVersion: entry.CurrentVersion,
				NewVersion:      entry.CurrentVersion,
				Status:          "skipped",
				RiskScore:       "—",
				ErrorCode:       "skipped by user",
			})
			r.printBatchProgressLine(i+1, payload.Total, entry, batchResult{Workspace: entry.Workspace, Status: "skipped"})
			continue
		case batchStop:
			results = append(results, batchResult{
				Workspace:       entry.Workspace,
				PreviousVersion: entry.CurrentVersion,
				NewVersion:      entry.CurrentVersion,
				Status:          "skipped",
				RiskScore:       "—",
				ErrorCode:       "batch stopped",
			})
			r.printBatchProgressLine(i+1, payload.Total, entry, batchResult{Workspace: entry.Workspace, Status: "skipped"})
			for j := i + 1; j < len(payload.Queue); j++ {
				rest := payload.Queue[j]
				results = append(results, batchResult{
					Workspace:       rest.Workspace,
					PreviousVersion: rest.CurrentVersion,
					NewVersion:      rest.CurrentVersion,
					Status:          "skipped",
					RiskScore:       "—",
					ErrorCode:       "batch stopped",
				})
			}
			fmt.Println()
			vaultYellow.Printf("  Batch stopped after %d/%d workspaces.\n", i+1, payload.Total)
			return results
		}

		res := r.runSingleUpgrade(ctx, payload, entry)
		r.printBatchProgressLine(i+1, payload.Total, entry, res)
		results = append(results, res)
	}
	fmt.Println()
	waypointTeal.Printf("  Batch complete — %d/%d workspaces processed.\n", len(payload.Queue), payload.Total)
	return results
}

// approveBatchWorkspace renders the per-workspace prompt and reads the user's
// decision. risk_flag workspaces always re-prompt — the auto-approve path is
// only honored for safe entries.
func (r *REPL) approveBatchWorkspace(idx, total int, entry batchQueueEntry) batchDecision {
	if r.batchAutoApprove && !entry.RiskFlag {
		fmt.Println()
		dimWhite.Printf("  [%d/%d] %s (%s → %s) — auto-approved\n", idx, total, entry.Workspace, entry.CurrentVersion, entry.TargetVersion)
		return batchApprove
	}

	fmt.Println()
	if entry.RiskFlag {
		details := []string{}
		if entry.ResourceCount > 50 {
			details = append(details, fmt.Sprintf("%d resources", entry.ResourceCount))
		}
		if entry.LastDestroys > 0 {
			details = append(details, fmt.Sprintf("last run had %d destruction(s)", entry.LastDestroys))
		}
		boundaryPink.Printf("  ⚠ High-risk workspace detected: %s", entry.Workspace)
		if len(details) > 0 {
			boundaryPink.Printf(" (%s)", strings.Join(details, ", "))
		}
		fmt.Println()
		boundaryPink.Println("  Risk score: HIGH — auto-pause overrides 'yes to all'.")
		vaultYellow.Println("  Type 'yes' to proceed, 'skip' to skip, or 'stop' to end batch.")
	} else {
		vaultYellow.Printf("  [%d/%d] Upgrade %s from %s to %s?\n", idx, total, entry.Workspace, entry.CurrentVersion, entry.TargetVersion)
		if entry.CVECount > 0 {
			dimWhite.Printf("    Resolves %d CVE(s)", entry.CVECount)
			if entry.HighestSeverity != "" {
				dimWhite.Printf(" — highest severity: %s", entry.HighestSeverity)
			}
			fmt.Println()
		}
		vaultYellow.Println("    yes / yes to all / skip / stop")
	}

	for {
		input := strings.ToLower(strings.TrimSpace(r.readLine()))
		switch input {
		case "yes", "y":
			return batchApprove
		case "yes to all", "all", "ya":
			if entry.RiskFlag {
				vaultYellow.Println("  'yes to all' is not allowed on high-risk workspaces — type 'yes', 'skip', or 'stop'.")
				continue
			}
			return batchApproveAll
		case "skip", "s", "no", "n":
			return batchSkip
		case "stop", "quit":
			return batchStop
		case "":
			return batchSkip
		default:
			vaultYellow.Println("  Enter yes, yes to all, skip, or stop.")
		}
	}
}

// runSingleUpgrade executes the upgrade chain for one workspace and returns
// the per-workspace result. Wraps version_upgrade → plan_analyze → run_apply
// with the existing tool helpers; relies on r.batchActive to short-circuit
// approveMutation for non-destructive applies.
func (r *REPL) runSingleUpgrade(ctx context.Context, payload batchQueuePayload, entry batchQueueEntry) batchResult {
	start := time.Now()
	res := batchResult{
		Workspace:       entry.Workspace,
		PreviousVersion: entry.CurrentVersion,
		NewVersion:      entry.TargetVersion,
		CVEsResolved:    entry.CVEIDs,
		RiskScore:       "Low",
	}

	upgradeArgs := map[string]string{
		"org":            payload.Org,
		"workspace":      entry.Workspace,
		"target_version": entry.TargetVersion,
	}
	upRes := tools.Call(ctx, "_hcp_tf_version_upgrade", upgradeArgs, r.cfg.TimeoutSeconds)
	if upRes.Err != nil {
		res.Status = "failed"
		res.ErrorCode = upRes.Err.ErrorCode
		res.NewVersion = entry.CurrentVersion
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	var upgradeOut struct {
		RunID  string `json:"run_id"`
		IsNoop bool   `json:"is_noop"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(upRes.Output, &upgradeOut)
	res.RunID = upgradeOut.RunID

	if upgradeOut.IsNoop {
		res.Status = "noop"
		res.RiskScore = "Low"
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	analyzeArgs := map[string]string{
		"org":       payload.Org,
		"workspace": entry.Workspace,
		"run_id":    upgradeOut.RunID,
	}
	analyze := tools.Call(ctx, "_hcp_tf_plan_analyze", analyzeArgs, r.cfg.TimeoutSeconds)
	if analyze.Err == nil {
		assessment := parseAssessment(analyze.Output)
		if assessment.riskLevel != "" {
			res.RiskScore = assessment.riskLevel
		}
		// Cache plan summary so destroysFromLastPlan() works for the apply
		// gate's destructive-confirmation path.
		r.mu.Lock()
		r.lastPlanSummary = analyze.Output
		r.mu.Unlock()
	}

	applyArgs := map[string]string{
		"org":       payload.Org,
		"workspace": entry.Workspace,
		"run_id":    upgradeOut.RunID,
		"comment":   fmt.Sprintf("tfpilot batch upgrade to %s", entry.TargetVersion),
	}
	applyRes := tools.Call(ctx, "_hcp_tf_run_apply", applyArgs, r.cfg.TimeoutSeconds)
	if applyRes.Err != nil {
		res.Status = "failed"
		res.ErrorCode = applyRes.Err.ErrorCode
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	res.Status = "applied"
	res.DurationMs = time.Since(start).Milliseconds()
	return res
}

func (r *REPL) printBatchProgressLine(idx, total int, entry batchQueueEntry, res batchResult) {
	prefix := fmt.Sprintf("  [%d/%d] %s", idx, total, entry.Workspace)
	switch res.Status {
	case "applied":
		waypointTeal.Printf("%s (%s → %s) ✓ applied — %s risk\n", prefix, entry.CurrentVersion, entry.TargetVersion, strings.ToUpper(res.RiskScore))
	case "noop":
		waypointTeal.Printf("%s (%s → %s) ✓ no-op — version constraint applied\n", prefix, entry.CurrentVersion, entry.TargetVersion)
	case "skipped":
		dimWhite.Printf("%s ⊘ skipped\n", prefix)
	case "failed":
		boundaryPink.Printf("%s (%s → %s) ✗ failed — %s\n", prefix, entry.CurrentVersion, entry.TargetVersion, res.ErrorCode)
	default:
		dimWhite.Printf("%s — %s\n", prefix, res.Status)
	}
}

// autoGenerateComplianceReport invokes _hcp_tf_compliance_report with the
// just-completed batch's results and prints the report path. Failures are
// surfaced but don't abort the REPL.
func (r *REPL) autoGenerateComplianceReport(ctx context.Context, payload batchQueuePayload, results []batchResult) {
	if len(results) == 0 {
		return
	}
	body, err := json.Marshal(results)
	if err != nil {
		boundaryPink.Printf("  ✗ compliance report: marshal results: %v\n", err)
		return
	}
	args := map[string]string{
		"org":            payload.Org,
		"results":        string(body),
		"target_version": payload.TargetVersion,
	}
	res := tools.Call(ctx, "_hcp_tf_compliance_report", args, r.cfg.TimeoutSeconds)
	if res.Err != nil {
		boundaryPink.Printf("  ✗ compliance report: %s\n", res.Err.Message)
		return
	}
	var out struct {
		ReportPath string `json:"report_path"`
	}
	if err := json.Unmarshal(res.Output, &out); err == nil && out.ReportPath != "" {
		fmt.Println()
		waypointTeal.Printf("  ✓ Compliance report written: %s\n", out.ReportPath)
		dimWhite.Println("  Type /report to regenerate.")
	}
}

// handleReport implements the /report slash command. Regenerates the
// compliance report from the most recent batch run without going through the
// agent. Errors surface in boundaryPink; success prints the new report path.
func (r *REPL) handleReport() {
	r.mu.Lock()
	results := r.lastBatchResults
	r.mu.Unlock()
	if len(results) == 0 {
		boundaryPink.Println("No batch results in this session. Run 'fix the rest' first.")
		return
	}
	body, err := json.Marshal(results)
	if err != nil {
		boundaryPink.Printf("compliance report: marshal results: %v\n", err)
		return
	}
	target := ""
	for _, r := range results {
		if r.NewVersion != "" {
			target = r.NewVersion
			break
		}
	}
	args := map[string]string{
		"org":            r.org,
		"results":        string(body),
		"target_version": target,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	res := tools.Call(ctx, "_hcp_tf_compliance_report", args, r.cfg.TimeoutSeconds)
	if res.Err != nil {
		boundaryPink.Printf("compliance report: %s\n", res.Err.Message)
		return
	}
	var out struct {
		ReportPath string `json:"report_path"`
		Summary    struct {
			Upgraded     int      `json:"upgraded"`
			Skipped      int      `json:"skipped"`
			Failed       int      `json:"failed"`
			Noop         int      `json:"noop"`
			CVEsResolved int      `json:"cves_resolved"`
			CVEIDs       []string `json:"cve_ids_resolved"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		boundaryPink.Printf("compliance report: parse output: %v\n", err)
		return
	}
	fmt.Println()
	waypointTeal.Printf("  ✓ Compliance report written: %s\n", out.ReportPath)
	dimWhite.Printf("  %d upgraded, %d no-op, %d skipped, %d failed", out.Summary.Upgraded, out.Summary.Noop, out.Summary.Skipped, out.Summary.Failed)
	if out.Summary.CVEsResolved > 0 {
		dimWhite.Printf(" — %d CVE(s) resolved: %s", out.Summary.CVEsResolved, strings.Join(out.Summary.CVEIDs, ", "))
	}
	fmt.Println()
}
