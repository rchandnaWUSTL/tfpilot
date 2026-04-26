package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/tfpilot/internal/config"
	"github.com/rchandnaWUSTL/tfpilot/internal/tools"
)

// defaultLatestTerraform is the fallback target Terraform version used when
// version_audit yields no parseable FixedIn semvers across at-risk workspaces.
// Bump this each release as the upstream stable line moves.
const defaultLatestTerraform = "1.14.9"

// HashiCorp brand colors, mirrored from internal/repl/repl.go so cmd/tfpilot
// does not have to import an internal package solely for color helpers.
var (
	tfPurple     = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(99), color.Bold)
	waypointTeal = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(44))
	vaultYellow  = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(220))
	boundaryPink = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(203))
	dimWhite     = color.New(color.FgWhite, color.Faint)
	whiteCol     = color.New(color.FgWhite)
)

// Mirrors of tool return shapes, narrowed to the fields watch mode reads.

type wmTopCVE struct {
	ID                 string `json:"id"`
	Severity           string `json:"severity"`
	Summary            string `json:"summary"`
	AffectedWorkspaces int    `json:"affected_workspaces"`
}

type wmComplianceSummary struct {
	Org                string     `json:"org"`
	ComplianceScore    *int       `json:"compliance_score"`
	ComplianceVerdict  string     `json:"compliance_verdict"`
	TotalWorkspaces    int        `json:"total_workspaces"`
	AtRiskWorkspaces   int        `json:"at_risk_workspaces"`
	CriticalWorkspaces int        `json:"critical_workspaces"`
	TopCVEs            []wmTopCVE `json:"top_cves"`
	CVEDataUnavailable bool       `json:"cve_data_unavailable"`
}

type wmAdvisory struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	FixedIn  string `json:"fixed_in"`
}

type wmVersionAuditEntry struct {
	TerraformVersion string       `json:"terraform_version"`
	Workspaces       []string     `json:"workspaces"`
	KnownCVEs        []wmAdvisory `json:"known_cves"`
}

type wmVersionAudit struct {
	Summary               []wmVersionAuditEntry `json:"version_summary"`
	LatestTerraformVer    string                `json:"latest_terraform_version"`
	WorkspacesAtRisk      int                   `json:"workspaces_at_risk"`
	CVEDataUnavailable    bool                  `json:"cve_data_unavailable"`
}

type wmQueueEntry struct {
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

type wmBatchUpgrade struct {
	Total int            `json:"total"`
	Queue []wmQueueEntry `json:"queue"`
}

type wmVersionUpgrade struct {
	Workspace     string `json:"workspace"`
	TargetVersion string `json:"target_version"`
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	IsNoop        bool   `json:"is_noop"`
	Message       string `json:"message"`
}

type wmComplianceReport struct {
	ReportPath string `json:"report_path"`
}

// wmBatchResult is the JSON shape consumed by _hcp_tf_compliance_report. We
// build it locally rather than exporting internal/repl.batchResult, because
// watch mode does not depend on the repl package at all.
type wmBatchResult struct {
	Workspace       string   `json:"workspace"`
	PreviousVersion string   `json:"previous_version"`
	NewVersion      string   `json:"new_version"`
	Status          string   `json:"status"`
	RiskScore       string   `json:"risk_score"`
	CVEsResolved    []string `json:"cves_resolved"`
	RunID           string   `json:"run_id"`
	ErrorCode       string   `json:"error_code,omitempty"`
	DurationMs      int64    `json:"duration_ms"`
}

func runWatchMode(ctx context.Context, cfg *config.Config, org string) error {
	printWatchBanner(cfg)
	dimWhite.Println("  Scanning org silently...")
	fmt.Println()

	// 1. Compliance summary
	summary, err := callComplianceSummary(ctx, cfg, org)
	if err != nil {
		return err
	}

	// Healthy short-circuit: no CVE data is treated as inconclusive, not healthy.
	if summary.ComplianceScore != nil && *summary.ComplianceScore == 100 && len(summary.TopCVEs) == 0 {
		waypointTeal.Println("  ✓ No vulnerabilities detected. Infrastructure is compliant.")
		fmt.Println()
		return nil
	}
	if summary.CVEDataUnavailable {
		boundaryPink.Println("  ⚠ CVE data unavailable. Cannot build a remediation plan.")
		fmt.Println()
		return nil
	}

	// 2. Version audit → target version + at-risk workspace list
	audit, err := callVersionAudit(ctx, cfg, org)
	if err != nil {
		return err
	}
	target := pickTargetVersion(audit)
	atRiskWorkspaces := collectAtRiskWorkspaces(audit)
	if len(atRiskWorkspaces) == 0 {
		waypointTeal.Println("  ✓ No vulnerable workspaces require remediation.")
		fmt.Println()
		return nil
	}

	// 3. Batch upgrade → priority queue
	batch, err := callBatchUpgrade(ctx, cfg, org, atRiskWorkspaces, target)
	if err != nil {
		return err
	}
	if len(batch.Queue) == 0 {
		waypointTeal.Println("  ✓ Nothing to upgrade.")
		fmt.Println()
		return nil
	}

	// 4. Lowest-risk-first re-sort (batch_upgrade returns highest-risk-first)
	queue := batch.Queue
	sort.SliceStable(queue, func(i, j int) bool {
		if queue[i].RiskFlag != queue[j].RiskFlag {
			return !queue[i].RiskFlag
		}
		if queue[i].ResourceCount != queue[j].ResourceCount {
			return queue[i].ResourceCount < queue[j].ResourceCount
		}
		return queue[i].Workspace < queue[j].Workspace
	})

	// 5. Aggregates for suggestion line
	totalResources := 0
	destructiveCount := 0
	for _, q := range queue {
		totalResources += q.ResourceCount
		if q.LastDestroys > 0 {
			destructiveCount++
		}
	}

	// In report mode, we skip the prompt and the execute loop entirely.
	if cfg.Mode == "report" {
		return emitReport(ctx, cfg, org, target, nil)
	}

	// 6. Render suggestion
	renderSuggestion(summary, queue, target, totalResources, destructiveCount)

	// 7. Approval gate
	choice := promptApproval()
	switch choice {
	case "n":
		dimWhite.Println("  Cancelled.")
		fmt.Println()
		return nil
	case "report":
		fmt.Println()
		return emitReport(ctx, cfg, org, target, nil)
	case "y":
		// fall through to execute
	default:
		dimWhite.Println("  Cancelled.")
		fmt.Println()
		return nil
	}

	// 8. Execute loop
	fmt.Println()
	waypointTeal.Println("  Executing upgrade plan...")
	fmt.Println()

	results := make([]wmBatchResult, 0, len(queue))
	successCount := 0
	total := len(queue)
	for i, entry := range queue {
		t0 := time.Now()
		out, callErr := callVersionUpgrade(ctx, cfg, org, entry.Workspace, target)
		duration := time.Since(t0)
		if callErr != nil {
			boundaryPink.Printf("  ✗ [%d/%d] %s — failed: %s\n", i+1, total, entry.Workspace, callErr.Error())
			results = append(results, wmBatchResult{
				Workspace:       entry.Workspace,
				PreviousVersion: entry.CurrentVersion,
				NewVersion:      target,
				Status:          "failed",
				CVEsResolved:    entry.CVEIDs,
				ErrorCode:       callErr.Error(),
				DurationMs:      duration.Milliseconds(),
			})
			continue
		}

		status := "applied"
		if out.IsNoop {
			status = "noop"
		}
		risk := "Low"
		if entry.RiskFlag {
			risk = "High"
		} else if entry.ResourceCount > 20 {
			risk = "Medium"
		}
		results = append(results, wmBatchResult{
			Workspace:       entry.Workspace,
			PreviousVersion: entry.CurrentVersion,
			NewVersion:      target,
			Status:          status,
			RiskScore:       risk,
			CVEsResolved:    entry.CVEIDs,
			RunID:           out.RunID,
			DurationMs:      duration.Milliseconds(),
		})
		successCount++

		if out.IsNoop {
			waypointTeal.Printf("  ✓ [%d/%d] %s — already on %s (no-op)\n", i+1, total, entry.Workspace, target)
		} else {
			waypointTeal.Printf("  ✓ [%d/%d] %s — upgrading to %s\n", i+1, total, entry.Workspace, target)
		}
	}

	// 9. Wrap-up
	fmt.Println()
	estimatedHours := int(math.Ceil(float64(successCount*3) / 60.0))
	if estimatedHours < 1 && successCount > 0 {
		estimatedHours = 1
	}
	waypointTeal.Printf("  ✓ Queued %d upgrade runs. Estimated completion: %d hours.\n", successCount, estimatedHours)
	waypointTeal.Println("  ✓ Audit log updated.")

	// 10. Compliance report
	return emitReport(ctx, cfg, org, target, results)
}

func printWatchBanner(cfg *config.Config) {
	tfRows := []string{
		"  ████████╗███████╗██████╗ ██╗██╗      ██████╗ ████████╗",
		"  ╚══██╔══╝██╔════╝██╔══██╗██║██║     ██╔═══██╗╚══██╔══╝",
		"     ██║   █████╗  ██████╔╝██║██║     ██║   ██║   ██║   ",
		"     ██║   ██╔══╝  ██╔═══╝ ██║██║     ██║   ██║   ██║   ",
		"     ██║   ██║     ██║     ██║███████╗╚██████╔╝   ██║   ",
		"     ╚═╝   ╚═╝     ╚═╝     ╚═╝╚══════╝ ╚═════╝    ╚═╝   ",
	}
	fmt.Println()
	for _, row := range tfRows {
		tfPurple.Println(row)
	}
	fmt.Println()
	whiteCol.Println("  AI-powered development for infrastructure-as-code")
	dimWhite.Println("  v1.9.0 • watch mode")
	fmt.Println()
	dimWhite.Println(strings.Repeat("-", utf8.RuneCountInString(tfRows[0])))
	fmt.Println()
	mode := cfg.Mode
	if mode == "" {
		mode = "suggest"
	}
	dimWhite.Printf("  model: %s  |  mode: watch/%s\n", cfg.Model, mode)
	fmt.Println()
}

func renderSuggestion(summary *wmComplianceSummary, queue []wmQueueEntry, target string, totalResources, destructiveCount int) {
	atRisk := summary.AtRiskWorkspaces
	if atRisk == 0 {
		atRisk = len(queue)
	}
	vaultYellow.Println("  💡 Suggestion:")
	whiteCol.Printf("  %d workspaces running vulnerable Terraform versions.\n", atRisk)
	if len(summary.TopCVEs) > 0 {
		top := summary.TopCVEs[0]
		whiteCol.Printf("  Top CVE: %s (%s) — %s\n", top.ID, top.Severity, top.Summary)
	}
	fmt.Println()
	whiteCol.Println("  Proposed fix: incremental upgrade plan, lowest-risk workspaces first.")
	whiteCol.Printf("  Target version: %s\n", target)
	whiteCol.Printf("  Blast radius: %d workspaces, %d total resources, %d destructive changes.\n",
		len(queue), totalResources, destructiveCount)
	fmt.Println()
	if destructiveCount > 0 {
		boundaryPink.Println("  ⚠ Warning: some workspaces have destructive changes. Review carefully.")
		fmt.Println()
	}
}

func promptApproval() string {
	reader := bufio.NewReader(os.Stdin)
	for attempt := 0; attempt < 3; attempt++ {
		whiteCol.Print("  Approve? [y/n/report] ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "n"
		}
		ans := strings.ToLower(strings.TrimSpace(line))
		switch ans {
		case "y", "yes":
			return "y"
		case "n", "no":
			return "n"
		case "report":
			return "report"
		}
		dimWhite.Println("  Please type y, n, or report.")
	}
	return "n"
}

func callComplianceSummary(ctx context.Context, cfg *config.Config, org string) (*wmComplianceSummary, error) {
	res := tools.Call(ctx, "_hcp_tf_compliance_summary", map[string]string{"org": org}, summaryTimeout(cfg))
	if res.Err != nil {
		return nil, fmt.Errorf("compliance scan failed: %s", res.Err.Message)
	}
	var out wmComplianceSummary
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, fmt.Errorf("compliance scan: parse: %w", err)
	}
	return &out, nil
}

func callVersionAudit(ctx context.Context, cfg *config.Config, org string) (*wmVersionAudit, error) {
	res := tools.Call(ctx, "_hcp_tf_version_audit", map[string]string{"org": org}, summaryTimeout(cfg))
	if res.Err != nil {
		return nil, fmt.Errorf("version audit failed: %s", res.Err.Message)
	}
	var out wmVersionAudit
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, fmt.Errorf("version audit: parse: %w", err)
	}
	return &out, nil
}

func callBatchUpgrade(ctx context.Context, cfg *config.Config, org string, workspaces []string, target string) (*wmBatchUpgrade, error) {
	res := tools.Call(ctx, "_hcp_tf_batch_upgrade", map[string]string{
		"org":            org,
		"workspaces":     strings.Join(workspaces, ","),
		"target_version": target,
		"mode":           "auto",
	}, summaryTimeout(cfg))
	if res.Err != nil {
		return nil, fmt.Errorf("batch upgrade queue: %s", res.Err.Message)
	}
	var out wmBatchUpgrade
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, fmt.Errorf("batch upgrade queue: parse: %w", err)
	}
	return &out, nil
}

func callVersionUpgrade(ctx context.Context, cfg *config.Config, org, workspace, target string) (*wmVersionUpgrade, error) {
	res := tools.Call(ctx, "_hcp_tf_version_upgrade", map[string]string{
		"org":            org,
		"workspace":      workspace,
		"target_version": target,
	}, upgradeTimeout(cfg))
	if res.Err != nil {
		return nil, fmt.Errorf("%s", res.Err.Message)
	}
	var out wmVersionUpgrade
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, fmt.Errorf("parse version_upgrade response: %w", err)
	}
	return &out, nil
}

func emitReport(ctx context.Context, cfg *config.Config, org, target string, results []wmBatchResult) error {
	if results == nil {
		results = []wmBatchResult{}
	}
	payload, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("marshal report payload: %w", err)
	}
	res := tools.Call(ctx, "_hcp_tf_compliance_report", map[string]string{
		"org":            org,
		"results":        string(payload),
		"target_version": target,
		"output_dir":     "~/.tfpilot/reports",
	}, summaryTimeout(cfg))
	if res.Err != nil {
		return fmt.Errorf("compliance report: %s", res.Err.Message)
	}
	var rep wmComplianceReport
	if err := json.Unmarshal(res.Output, &rep); err != nil {
		return fmt.Errorf("parse compliance report response: %w", err)
	}
	fmt.Println()
	waypointTeal.Printf("  ✓ Compliance report generated: %s\n", rep.ReportPath)
	fmt.Println()
	return nil
}

// pickTargetVersion walks every CVE's FixedIn across at-risk versions and
// returns the highest semver. Falls back to the audit's
// latest_terraform_version, then to defaultLatestTerraform if neither parses.
func pickTargetVersion(audit *wmVersionAudit) string {
	var best []int
	bestStr := ""
	for _, entry := range audit.Summary {
		for _, cve := range entry.KnownCVEs {
			parsed, ok := parseSemver(strings.TrimSpace(cve.FixedIn))
			if !ok {
				continue
			}
			if best == nil || compareSemver(parsed, best) > 0 {
				best = parsed
				bestStr = cve.FixedIn
			}
		}
	}
	if bestStr != "" {
		return bestStr
	}
	if v := strings.TrimSpace(audit.LatestTerraformVer); v != "" {
		return v
	}
	return defaultLatestTerraform
}

func collectAtRiskWorkspaces(audit *wmVersionAudit) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range audit.Summary {
		if len(entry.KnownCVEs) == 0 {
			continue
		}
		for _, ws := range entry.Workspaces {
			ws = strings.TrimSpace(ws)
			if ws == "" || seen[ws] {
				continue
			}
			seen[ws] = true
			out = append(out, ws)
		}
	}
	sort.Strings(out)
	return out
}

func parseSemver(s string) ([]int, bool) {
	if s == "" {
		return nil, false
	}
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, "-", 2) // strip prerelease suffix
	parts = strings.Split(parts[0], ".")
	if len(parts) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func compareSemver(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func summaryTimeout(cfg *config.Config) int {
	if cfg.TimeoutSeconds < 30 {
		return 60
	}
	return cfg.TimeoutSeconds
}

func upgradeTimeout(cfg *config.Config) int {
	if cfg.TimeoutSeconds < 60 {
		return 120
	}
	return cfg.TimeoutSeconds
}
