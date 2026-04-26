package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// orgTimelineCall implements _hcp_tf_org_timeline. It fans out across every
// workspace in the org with at least one resource, pulls each workspace's
// recent runs, optionally enriches with plan counts, and returns the merged
// timeline sorted newest-first along with anomaly flags.
func orgTimelineCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_org_timeline", Args: args}

	if err := require(args, "org"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]

	hours := 24
	if h := strings.TrimSpace(args["hours"]); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
			hours = n
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	filter := map[string]bool{}
	if f := strings.TrimSpace(args["workspace_filter"]); f != "" {
		for _, name := range strings.Split(f, ",") {
			n := strings.TrimSpace(name)
			if n != "" {
				filter[n] = true
			}
		}
	}

	wsRaw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "list", "-org="+org, "-output=json")
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}
	var workspaces []map[string]any
	if err := json.Unmarshal(wsRaw, &workspaces); err != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: "decode workspace list: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}

	candidates := []string{}
	for _, ws := range workspaces {
		name := firstStringField(ws, "name", "Name")
		if name == "" {
			continue
		}
		if len(filter) > 0 && !filter[name] {
			continue
		}
		candidates = append(candidates, name)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		timeline []map[string]any
		active   int
	)

	for _, name := range candidates {
		wg.Add(1)
		go func(ws string) {
			defer wg.Done()

			readRaw, rerr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "read", "-org="+org, "-name="+ws, "-output=json")
			if rerr != nil {
				return
			}
			var detail map[string]any
			if err := json.Unmarshal(readRaw, &detail); err != nil {
				return
			}
			rc := firstIntField(detail, "ResourceCount", "resource-count", "resource_count")
			if rc == 0 {
				return
			}

			runsRaw, rferr := fetchHCPTFJSON(ctx, timeoutSec, "run", "list",
				"-org="+org, "-workspace="+ws, "-output=json")
			if rferr != nil {
				return
			}
			var runs []map[string]any
			if err := json.Unmarshal(runsRaw, &runs); err != nil {
				return
			}

			localActive := false
			for _, run := range runs {
				createdStr := firstStringField(run, "CreatedAt", "created-at", "created_at")
				createdAt, parseErr := time.Parse(time.RFC3339, createdStr)
				if parseErr != nil {
					continue
				}
				if createdAt.Before(cutoff) {
					continue
				}
				localActive = true

				runID := firstStringField(run, "ID", "id")
				status := firstStringField(run, "Status", "status")
				message := firstStringField(run, "Message", "message")
				source := firstStringField(run, "Source", "source")

				entry := map[string]any{
					"workspace":        ws,
					"run_id":           runID,
					"status":           status,
					"message":          message,
					"created_at":       createdAt.UTC().Format(time.RFC3339),
					"created_at_human": humanRelative(createdAt),
					"triggered_by":     normalizeRunSource(source),
					"has_changes":      false,
					"additions":        0,
					"changes":          0,
					"destructions":     0,
				}

				if runID != "" && (status == "applied" || status == "errored" || status == "planned" || status == "planned_and_finished" || status == "policy_checked") {
					if planRaw, perr := fetchHCPTFJSON(ctx, timeoutSec, "plan", "read", "-run-id="+runID, "-output=json"); perr == nil {
						counts := decodePlanCounts(planRaw)
						entry["additions"] = counts.additions
						entry["changes"] = counts.changes
						entry["destructions"] = counts.destructions
						entry["has_changes"] = counts.additions+counts.changes+counts.destructions > 0
					}
				}

				mu.Lock()
				timeline = append(timeline, entry)
				mu.Unlock()
			}

			if localActive {
				mu.Lock()
				active++
				mu.Unlock()
			}
		}(name)
	}
	wg.Wait()

	sort.SliceStable(timeline, func(i, j int) bool {
		return firstStringField(timeline[i], "created_at") > firstStringField(timeline[j], "created_at")
	})

	anomalies := detectAnomalies(timeline)

	payload := map[string]any{
		"org":                      org,
		"hours":                    hours,
		"total_changes":            len(timeline),
		"workspaces_with_activity": active,
		"timeline":                 timeline,
		"anomalies":                anomalies,
	}
	out, mErr := json.Marshal(payload)
	if mErr != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: mErr.Error()}
		result.Duration = time.Since(start)
		return result
	}
	result.Output = out
	result.Duration = time.Since(start)
	return result
}

func normalizeRunSource(s string) string {
	switch strings.ToLower(s) {
	case "tfe-api", "tfc-api", "api":
		return "api"
	case "tfe-ui", "tfc-ui", "ui":
		return "ui"
	case "tfe-configuration-version", "tfe-vcs", "vcs":
		return "vcs"
	case "tfe-cli", "cli":
		return "cli"
	case "":
		return ""
	default:
		return s
	}
}

// detectAnomalies flags suspicious patterns across the merged timeline.
func detectAnomalies(timeline []map[string]any) []map[string]any {
	out := []map[string]any{}
	if len(timeline) == 0 {
		return out
	}

	type runRef struct {
		ws       string
		when     time.Time
		destroys int
		errored  bool
	}
	refs := make([]runRef, 0, len(timeline))
	for _, e := range timeline {
		t, err := time.Parse(time.RFC3339, firstStringField(e, "created_at"))
		if err != nil {
			continue
		}
		refs = append(refs, runRef{
			ws:       firstStringField(e, "workspace"),
			when:     t,
			destroys: firstIntField(e, "destructions"),
			errored:  firstStringField(e, "status") == "errored",
		})
	}
	if len(refs) == 0 {
		return out
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].when.Before(refs[j].when) })

	const window = 30 * time.Minute
	used := map[int]bool{}
	for i := range refs {
		if used[i] {
			continue
		}
		clusterWS := map[string]bool{refs[i].ws: true}
		clusterEnd := refs[i].when
		members := []int{i}
		for j := i + 1; j < len(refs); j++ {
			if refs[j].when.Sub(refs[i].when) > window {
				break
			}
			clusterWS[refs[j].ws] = true
			clusterEnd = refs[j].when
			members = append(members, j)
		}
		if len(clusterWS) >= 2 {
			for _, m := range members {
				used[m] = true
			}
			ws := make([]string, 0, len(clusterWS))
			for k := range clusterWS {
				ws = append(ws, k)
			}
			sort.Strings(ws)
			out = append(out, map[string]any{
				"type":         "multiple_changes_in_window",
				"description":  fmt.Sprintf("%d runs across %s within 30 minutes", len(members), strings.Join(ws, " and ")),
				"workspaces":   ws,
				"window_start": refs[i].when.UTC().Format(time.RFC3339),
				"window_end":   clusterEnd.UTC().Format(time.RFC3339),
			})
		}
	}

	failures := map[string]int{}
	for _, r := range refs {
		if r.errored {
			failures[r.ws]++
		}
	}
	for ws, n := range failures {
		if n >= 2 {
			out = append(out, map[string]any{
				"type":        "repeated_failure",
				"description": fmt.Sprintf("%s failed %d times in the window", ws, n),
				"workspaces":  []string{ws},
			})
		}
	}

	for _, r := range refs {
		if r.destroys > 0 {
			out = append(out, map[string]any{
				"type":         "unexpected_destruction",
				"description":  fmt.Sprintf("%s destroyed %d resource(s) at %s", r.ws, r.destroys, humanRelative(r.when)),
				"workspaces":   []string{r.ws},
				"window_start": r.when.UTC().Format(time.RFC3339),
				"window_end":   r.when.UTC().Format(time.RFC3339),
			})
		}
	}

	for _, r := range refs {
		h := r.when.UTC().Hour()
		if h >= 22 || h < 6 {
			out = append(out, map[string]any{
				"type":         "off_hours_change",
				"description":  fmt.Sprintf("%s changed at %02d:00 UTC (off-hours)", r.ws, h),
				"workspaces":   []string{r.ws},
				"window_start": r.when.UTC().Format(time.RFC3339),
				"window_end":   r.when.UTC().Format(time.RFC3339),
			})
		}
	}

	return out
}

// humanRelative returns a short relative-time string ("2 hours ago",
// "3 days ago"). Used by both the timeline tool and drift detection.
func humanRelative(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hrs := int(d.Hours())
		if hrs == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hrs)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := int(d.Hours() / (24 * 7))
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}
