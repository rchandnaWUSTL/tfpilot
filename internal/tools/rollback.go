package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// rollbackCall implements _hcp_tf_rollback. It picks (or accepts) a previous
// applied run, fetches that run's configuration-version-id from the HCP
// Terraform JSON:API, and creates a new run against that configuration —
// effectively re-applying the prior config. The new run goes through the
// standard plan_analyze + apply gate before any infrastructure changes
// happen, so this tool only triggers a plan; the actual mutation is gated
// downstream.
func rollbackCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_rollback", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]
	runIDArg := strings.TrimSpace(args["run_id"])

	wsRaw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "read",
		"-org="+org, "-name="+workspace, "-include=current_run", "-output=json")
	if ferr != nil {
		result.Err = ferr
		result.Duration = time.Since(start)
		return result
	}
	var wsDetail map[string]any
	if err := json.Unmarshal(wsRaw, &wsDetail); err != nil {
		result.Err = &ToolError{ErrorCode: "marshal_error", Message: "decode workspace read: " + err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	wsID := firstStringField(wsDetail, "ID", "id")
	if wsID == "" {
		result.Err = &ToolError{ErrorCode: "execution_error", Message: "workspace read did not return a workspace ID"}
		result.Duration = time.Since(start)
		return result
	}
	currentRunID := firstStringField(wsDetail, "CurrentRunID", "current-run-id", "current_run_id")

	token := readTFCToken()
	if token == "" {
		result.Err = &ToolError{
			ErrorCode: "execution_error",
			Message:   "no Terraform Cloud token in ~/.terraform.d/credentials.tfrc.json — run `terraform login` to authenticate",
			Retryable: false,
		}
		result.Duration = time.Since(start)
		return result
	}

	targetRunID, targetCreatedAt, perr := pickRollbackTarget(ctx, timeoutSec, org, workspace, currentRunID, runIDArg)
	if perr != nil {
		result.Err = perr
		result.Duration = time.Since(start)
		return result
	}

	cvID, cerr := fetchRunConfigVersionID(ctx, token, targetRunID, timeoutSec)
	if cerr != nil {
		result.Err = cerr
		result.Duration = time.Since(start)
		return result
	}
	if cvID == "" {
		result.Err = &ToolError{
			ErrorCode: "rollback_unavailable",
			Message:   fmt.Sprintf("run %s has no associated configuration version — cannot rollback to it", targetRunID),
			Retryable: false,
		}
		result.Duration = time.Since(start)
		return result
	}

	newRunID, perr := createRollbackRun(ctx, token, wsID, cvID, targetRunID, timeoutSec)
	if perr != nil {
		result.Err = perr
		result.Duration = time.Since(start)
		return result
	}

	payload := map[string]any{
		"workspace":                  workspace,
		"org":                        org,
		"rolled_back_to_run_id":      targetRunID,
		"rolled_back_to_created_at":  targetCreatedAt,
		"rolled_back_to_human":       humanRelative(parseTimeOrZero(targetCreatedAt)),
		"new_run_id":                 newRunID,
		"status":                     "queued",
		"next_step":                  "Call _hcp_tf_plan_analyze with run_id to assess blast radius, then call _hcp_tf_run_apply to complete the rollback after user confirms.",
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

// pickRollbackTarget resolves the run we are reverting to. When run_id is
// supplied we verify it exists and is `applied`; otherwise we pick the most
// recent `applied` run that is not the workspace's current run.
func pickRollbackTarget(ctx context.Context, timeoutSec int, org, workspace, currentRunID, requested string) (string, string, *ToolError) {
	runsRaw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "run", "list",
		"-org="+org, "-workspace="+workspace, "-output=json")
	if ferr != nil {
		return "", "", ferr
	}
	var runs []map[string]any
	if err := json.Unmarshal(runsRaw, &runs); err != nil {
		return "", "", &ToolError{ErrorCode: "marshal_error", Message: "decode run list: " + err.Error()}
	}

	type runRef struct {
		id      string
		created string
		when    time.Time
		status  string
	}
	parsed := make([]runRef, 0, len(runs))
	for _, r := range runs {
		id := firstStringField(r, "ID", "id")
		status := firstStringField(r, "Status", "status")
		created := firstStringField(r, "CreatedAt", "created-at", "created_at")
		t, _ := time.Parse(time.RFC3339, created)
		parsed = append(parsed, runRef{id: id, created: created, when: t, status: status})
	}

	if requested != "" {
		for _, r := range parsed {
			if r.id == requested {
				if r.status != "applied" {
					return "", "", &ToolError{
						ErrorCode: "rollback_unavailable",
						Message:   fmt.Sprintf("run %s has status %q — only `applied` runs can be used as rollback targets", requested, r.status),
						Retryable: false,
					}
				}
				return r.id, r.created, nil
			}
		}
		return "", "", &ToolError{
			ErrorCode: "rollback_unavailable",
			Message:   fmt.Sprintf("run %s not found in workspace %s", requested, workspace),
			Retryable: false,
		}
	}

	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].when.After(parsed[j].when) })
	for _, r := range parsed {
		if r.status != "applied" {
			continue
		}
		if r.id == currentRunID {
			continue
		}
		return r.id, r.created, nil
	}
	return "", "", &ToolError{
		ErrorCode: "rollback_unavailable",
		Message:   fmt.Sprintf("no prior `applied` run available for workspace %s", workspace),
		Retryable: false,
	}
}

// fetchRunConfigVersionID extracts data.relationships.configuration-version.data.id
// from GET /api/v2/runs/<id>.
func fetchRunConfigVersionID(ctx context.Context, token, runID string, timeoutSec int) (string, *ToolError) {
	url := "https://app.terraform.io/api/v2/runs/" + runID
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", &ToolError{ErrorCode: "execution_error", Message: "build run request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &ToolError{ErrorCode: "execution_error", Message: "fetch run: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", &ToolError{
			ErrorCode: "execution_error",
			Message:   fmt.Sprintf("run endpoint returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)),
			Retryable: resp.StatusCode >= 500,
		}
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return "", &ToolError{ErrorCode: "execution_error", Message: "read run body: " + rerr.Error()}
	}
	var doc struct {
		Data struct {
			Relationships struct {
				ConfigurationVersion struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"configuration-version"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", &ToolError{ErrorCode: "marshal_error", Message: "decode run: " + err.Error()}
	}
	return doc.Data.Relationships.ConfigurationVersion.Data.ID, nil
}

// createRollbackRun POSTs /api/v2/runs with the previous configuration
// version. Returns the new run's ID. The run is queued for manual apply
// (plan-only=false, auto-apply=false) so it reaches `planned` and waits for
// the user to approve via the existing _hcp_tf_run_apply gate — sending
// plan-only=true here would land the run in `planned_and_finished` and HCP
// Terraform would reject any apply with a transition error.
func createRollbackRun(ctx context.Context, token, wsID, cvID, sourceRunID string, timeoutSec int) (string, *ToolError) {
	body := map[string]any{
		"data": map[string]any{
			"type": "runs",
			"attributes": map[string]any{
				"message":    fmt.Sprintf("tfpilot rollback to run %s", sourceRunID),
				"is-destroy": false,
				"plan-only":  false,
				"auto-apply": false,
			},
			"relationships": map[string]any{
				"workspace": map[string]any{
					"data": map[string]any{"type": "workspaces", "id": wsID},
				},
				"configuration-version": map[string]any{
					"data": map[string]any{"type": "configuration-versions", "id": cvID},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", &ToolError{ErrorCode: "marshal_error", Message: "encode run body: " + err.Error()}
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, "https://app.terraform.io/api/v2/runs", bytes.NewReader(raw))
	if err != nil {
		return "", &ToolError{ErrorCode: "execution_error", Message: "build run create request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &ToolError{ErrorCode: "execution_error", Message: "post run: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ToolError{
			ErrorCode: "execution_error",
			Message:   fmt.Sprintf("create run returned HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 240)),
			Retryable: resp.StatusCode >= 500,
		}
	}
	var doc struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &doc); err != nil {
		return "", &ToolError{ErrorCode: "marshal_error", Message: "decode run create: " + err.Error()}
	}
	return doc.Data.ID, nil
}

func parseTimeOrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
