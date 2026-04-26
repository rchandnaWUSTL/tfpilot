package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)


// incidentSummaryCall implements _hcp_tf_incident_summary. It is a pure
// transformation: the agent passes the JSON outputs of an earlier
// _hcp_tf_org_timeline call and an earlier _hcp_tf_drift_detect call (plus
// optionally the run ID returned by _hcp_tf_rollback) and we synthesize a
// markdown postmortem written to ~/.tfpilot/incidents/.
//
// The tool itself does not call HCP Terraform — it only reads what the
// agent has already gathered and writes a report to the local audit
// directory, mirroring how audit.log lives under ~/.tfpilot.
func incidentSummaryCall(_ context.Context, args map[string]string, _ int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_incident_summary", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	timeline := parseTimelinePayload(args["timeline_data"])
	drift := parseDriftPayload(args["drift_data"])
	rollbackRun := strings.TrimSpace(args["rollback_run_id"])

	report := buildIncidentReport(org, workspace, timeline, drift, rollbackRun)

	dir, derr := incidentsDir()
	if derr != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "create incidents dir: " + derr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	now := time.Now().UTC()
	base := fmt.Sprintf("%s-%s.md", now.Format("2006-01-02"), sanitizeFilename(workspace))
	path := filepath.Join(dir, base)
	if _, err := os.Stat(path); err == nil {
		path = filepath.Join(dir, fmt.Sprintf("%s-%s-%s.md", now.Format("2006-01-02"), sanitizeFilename(workspace), now.Format("150405")))
	}
	if err := os.WriteFile(path, []byte(report.markdown), 0600); err != nil {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "write incident report: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	payload := map[string]any{
		"report_path":               path,
		"incident_duration_minutes": report.durationMinutes,
		"root_cause":                report.rootCause,
		"affected_workspaces":       report.affected,
		"report_markdown":           report.markdown,
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

// timelineEntry mirrors the shape of one element in _hcp_tf_org_timeline
// output. We only decode the fields we need for the report.
type timelineEntry struct {
	Workspace      string `json:"workspace"`
	RunID          string `json:"run_id"`
	Status         string `json:"status"`
	Message        string `json:"message"`
	CreatedAt      string `json:"created_at"`
	CreatedAtHuman string `json:"created_at_human"`
	TriggeredBy    string `json:"triggered_by"`
	Additions      int    `json:"additions"`
	Changes        int    `json:"changes"`
	Destructions   int    `json:"destructions"`
}

type timelinePayload struct {
	Org       string          `json:"org"`
	Hours     int             `json:"hours"`
	Timeline  []timelineEntry `json:"timeline"`
	Anomalies []struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"anomalies"`
}

type driftPayload struct {
	Workspace        string `json:"workspace"`
	Drifted          bool   `json:"drifted"`
	AssessmentStatus string `json:"assessment_status"`
	Summary          string `json:"summary"`
	LastAssessedAt   string `json:"last_assessment_at"`
	ResourcesDrifted int    `json:"resources_drifted"`
	DriftedResources []struct {
		Address    string `json:"address"`
		Provider   string `json:"provider"`
		ChangeType string `json:"change_type"`
	} `json:"drifted_resources"`
}

type incidentReport struct {
	markdown        string
	durationMinutes int
	rootCause       string
	affected        []string
}

func parseTimelinePayload(raw string) timelinePayload {
	var p timelinePayload
	if strings.TrimSpace(raw) == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

func parseDriftPayload(raw string) driftPayload {
	var p driftPayload
	if strings.TrimSpace(raw) == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

func buildIncidentReport(org, workspace string, timeline timelinePayload, drift driftPayload, rollbackRunID string) incidentReport {
	now := time.Now().UTC()

	affected := map[string]bool{workspace: true}
	for _, e := range timeline.Timeline {
		if e.Workspace != "" {
			affected[e.Workspace] = true
		}
	}
	affectedList := make([]string, 0, len(affected))
	for k := range affected {
		affectedList = append(affectedList, k)
	}
	sort.Strings(affectedList)

	var incidentStart time.Time
	for _, e := range timeline.Timeline {
		t, err := time.Parse(time.RFC3339, e.CreatedAt)
		if err != nil {
			continue
		}
		if incidentStart.IsZero() || t.Before(incidentStart) {
			incidentStart = t
		}
	}

	incidentEnd := now
	if rollbackRunID != "" {
		for _, e := range timeline.Timeline {
			if e.RunID == rollbackRunID {
				if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
					incidentEnd = t
				}
				break
			}
		}
	}
	durationMin := 0
	if !incidentStart.IsZero() && incidentEnd.After(incidentStart) {
		durationMin = int(incidentEnd.Sub(incidentStart).Minutes())
	}

	rootCause := inferRootCause(drift)
	totalDestroyed := 0
	totalChanged := 0
	for _, e := range timeline.Timeline {
		totalDestroyed += e.Destructions
		totalChanged += e.Additions + e.Changes
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Incident Report — %s — %s\n\n", workspace, now.Format("2006-01-02"))

	b.WriteString("## Summary\n")
	b.WriteString(buildSummaryParagraph(workspace, drift, timeline, rollbackRunID, durationMin))
	b.WriteString("\n\n")

	b.WriteString("## Timeline\n\n")
	if len(timeline.Timeline) == 0 {
		b.WriteString("_No runs recorded in the lookback window._\n\n")
	} else {
		b.WriteString("| Time | Workspace | Event | Triggered By |\n")
		b.WriteString("|------|-----------|-------|--------------|\n")
		ordered := append([]timelineEntry(nil), timeline.Timeline...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt < ordered[j].CreatedAt })
		for _, e := range ordered {
			when := e.CreatedAtHuman
			if when == "" {
				when = e.CreatedAt
			}
			event := fmt.Sprintf("%s — %s", e.Status, formatCounts(e))
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", when, e.Workspace, event, fallback(e.TriggeredBy, "—")))
		}
		if drift.Drifted {
			b.WriteString(fmt.Sprintf("| %s | %s | Drift detected — %d resource(s) | HCP Terraform assessment |\n", fallbackHuman(drift.LastAssessedAt), workspace, drift.ResourcesDrifted))
		}
		if rollbackRunID != "" {
			b.WriteString(fmt.Sprintf("| %s | %s | Rollback run created (%s) | tfpilot |\n", "now", workspace, rollbackRunID))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Root Cause\n")
	b.WriteString(rootCause)
	b.WriteString("\n\n")

	b.WriteString("## Impact\n")
	b.WriteString(fmt.Sprintf("- Affected workspaces: %s\n", strings.Join(affectedList, ", ")))
	b.WriteString(fmt.Sprintf("- Resources changed in window: %d\n", totalChanged))
	b.WriteString(fmt.Sprintf("- Resources destroyed in window: %d\n", totalDestroyed))
	if drift.ResourcesDrifted > 0 {
		b.WriteString(fmt.Sprintf("- Drifted resources: %d\n", drift.ResourcesDrifted))
	}
	if durationMin > 0 {
		b.WriteString(fmt.Sprintf("- Duration: %d minutes\n", durationMin))
	}
	b.WriteString("- Data loss: None observed\n\n")

	b.WriteString("## Resolution\n")
	if rollbackRunID != "" {
		b.WriteString(fmt.Sprintf("Rollback run %s queued in HCP Terraform (review blast radius before applying).\n\n", rollbackRunID))
	} else {
		b.WriteString("No rollback applied during this report. Review the timeline and drift findings before deciding on remediation.\n\n")
	}

	b.WriteString("## Action Items\n")
	b.WriteString("- [ ] Confirm who initiated the unexpected change and whether it was authorized\n")
	if drift.Drifted {
		b.WriteString("- [ ] Enable drift-detection alerts for the affected workspace(s)\n")
		b.WriteString("- [ ] Consider a Sentinel policy to block the resource type that drifted\n")
	}
	if len(timeline.Anomalies) > 0 {
		b.WriteString("- [ ] Review the anomaly findings below and update runbooks accordingly\n")
		for _, a := range timeline.Anomalies {
			b.WriteString(fmt.Sprintf("  - %s: %s\n", a.Type, a.Description))
		}
	}
	b.WriteString("- [ ] Schedule a follow-up review in 7 days to confirm no regression\n")

	return incidentReport{
		markdown:        b.String(),
		durationMinutes: durationMin,
		rootCause:       rootCause,
		affected:        affectedList,
	}
}

func inferRootCause(drift driftPayload) string {
	if !drift.Drifted || len(drift.DriftedResources) == 0 {
		if drift.AssessmentStatus == "error" && drift.Summary != "" {
			return drift.Summary
		}
		return "Root cause not yet determined — no drift was reported. Likely cause is a recent Terraform run; review the timeline above."
	}
	risky := []string{}
	for _, r := range drift.DriftedResources {
		addrLower := strings.ToLower(r.Address)
		if strings.Contains(addrLower, "security_group") ||
			strings.Contains(addrLower, "network_acl") ||
			strings.Contains(addrLower, "iam_") ||
			strings.Contains(addrLower, "_role") ||
			strings.Contains(addrLower, "_policy") {
			risky = append(risky, r.Address)
		}
	}
	if len(risky) > 0 {
		return fmt.Sprintf("Likely manual change outside Terraform: %s. Resources of this kind (security groups, network ACLs, IAM roles/policies) are commonly modified directly in the cloud console during incident response, which causes drift on the next assessment.", strings.Join(risky, ", "))
	}
	addresses := make([]string, 0, len(drift.DriftedResources))
	for _, r := range drift.DriftedResources {
		addresses = append(addresses, r.Address)
	}
	return fmt.Sprintf("Drift detected on %s. Investigate whether the change was made through Terraform or directly against the provider.", strings.Join(addresses, ", "))
}

func buildSummaryParagraph(workspace string, drift driftPayload, timeline timelinePayload, rollbackRun string, durationMin int) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("Workspace %s was investigated for an incident.", workspace))
	if len(timeline.Timeline) > 0 {
		parts = append(parts, fmt.Sprintf("The change-timeline scan covered %d hour(s) and surfaced %d run(s) across %d workspace(s).", timeline.Hours, len(timeline.Timeline), len(workspaceSet(timeline.Timeline))))
	}
	if drift.Drifted {
		parts = append(parts, fmt.Sprintf("Health assessment reports %d resource(s) drifted from state.", drift.ResourcesDrifted))
	} else if drift.AssessmentStatus == "ok" {
		parts = append(parts, "No drift was detected at last health assessment.")
	}
	if rollbackRun != "" {
		parts = append(parts, fmt.Sprintf("A rollback run was queued (%s); blast radius must be reviewed before applying.", rollbackRun))
	}
	if durationMin > 0 {
		parts = append(parts, fmt.Sprintf("Estimated incident duration: %d minute(s).", durationMin))
	}
	return strings.Join(parts, " ")
}

func workspaceSet(entries []timelineEntry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		if e.Workspace != "" {
			m[e.Workspace] = true
		}
	}
	return m
}

func formatCounts(e timelineEntry) string {
	if e.Additions == 0 && e.Changes == 0 && e.Destructions == 0 {
		return fallback(e.Message, "no plan counts available")
	}
	return fmt.Sprintf("+%d ~%d -%d", e.Additions, e.Changes, e.Destructions)
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func fallbackHuman(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return humanRelative(t)
}

// incidentsDir mirrors auditLogPath: lazily creates ~/.tfpilot/incidents
// with 0700 perms and returns the directory path.
func incidentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".tfpilot", "incidents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// sanitizeFilename strips characters that are awkward in filenames so the
// workspace name slots cleanly into the report basename.
func sanitizeFilename(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r == '_' || r == '-':
			out = append(out, r)
		case r >= 'a' && r <= 'z':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r)
		case r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "workspace"
	}
	return string(out)
}
