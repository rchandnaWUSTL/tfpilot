package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// driftDetectCall implements _hcp_tf_drift_detect by hitting the JSON:API
// endpoint GET /api/v2/workspaces/<id>/current-assessment-result. The previous
// implementation shelled out to `hcptf assessmentresult list`, which routes to
// a non-existent endpoint and always 404s.
//
// Flow:
//  1. `hcptf workspace read` to resolve workspace name → workspace ID.
//  2. Bearer token via readTFCToken().
//  3. GET current-assessment-result. A 404 means the workspace does not have
//     health assessments enabled, which is a normal state (not an error) — we
//     return assessments_enabled=false so the model can explain that.
//  4. When the assessment reports drift, fetch data.links.json-output and
//     extract every resource_change whose actions list is anything other than
//     ["no-op"] — those addresses are the drifted resources.
func driftDetectCall(ctx context.Context, args map[string]string, timeoutSec int) *CallResult {
	start := time.Now()
	result := &CallResult{ToolName: "_hcp_tf_drift_detect", Args: args}

	if err := require(args, "org", "workspace"); err != nil {
		result.Err = &ToolError{ErrorCode: "invalid_tool", Message: err.Error()}
		result.Duration = time.Since(start)
		return result
	}
	org := args["org"]
	workspace := args["workspace"]

	wsRaw, ferr := fetchHCPTFJSON(ctx, timeoutSec, "workspace", "read",
		"-org="+org, "-name="+workspace, "-output=json")
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

	assessment, derr := fetchCurrentAssessment(ctx, token, wsID, timeoutSec)
	if derr != nil {
		result.Err = derr
		result.Duration = time.Since(start)
		return result
	}

	// 404 — the workspace has assessments disabled or none has run yet.
	if assessment == nil {
		payload := map[string]any{
			"workspace":           workspace,
			"org":                 org,
			"assessments_enabled": false,
			"message":             "No health assessment is available for this workspace. Enable health assessments in the workspace settings, or wait for the next scheduled assessment to run.",
		}
		out, _ := json.Marshal(payload)
		result.Output = json.RawMessage(out)
		result.Duration = time.Since(start)
		return result
	}

	attrs, _ := assessment["attributes"].(map[string]any)
	links, _ := assessment["links"].(map[string]any)

	drifted := boolField(attrs, "drifted")
	resourcesDrifted := intField(attrs, "resources-drifted")
	resourcesUndrifted := intField(attrs, "resources-undrifted")
	succeeded := boolField(attrs, "succeeded")
	createdAt := stringField(attrs, "created-at")
	errMsg := stringField(attrs, "error-message")
	asmtID := stringField(assessment, "id")

	driftedResources := []driftedResource{}
	if drifted && resourcesDrifted > 0 {
		jsonOutPath := stringField(links, "json-output")
		if jsonOutPath != "" {
			driftedResources = fetchDriftedResources(ctx, token, jsonOutPath, timeoutSec)
		}
	}
	driftedAddresses := make([]string, 0, len(driftedResources))
	for _, r := range driftedResources {
		driftedAddresses = append(driftedAddresses, r.Address)
	}

	lastAssessedAt, _ := time.Parse(time.RFC3339, createdAt)
	lastAssessedHuman := ""
	if !lastAssessedAt.IsZero() {
		lastAssessedHuman = humanRelative(lastAssessedAt)
	}

	assessmentStatus := "ok"
	switch {
	case errMsg != "" || !succeeded:
		assessmentStatus = "error"
	case drifted:
		assessmentStatus = "drifted"
	}

	summary := buildDriftSummary(assessmentStatus, drifted, resourcesDrifted, resourcesUndrifted, lastAssessedHuman, errMsg)

	payload := map[string]any{
		"workspace":           workspace,
		"org":                 org,
		"assessments_enabled": true,
		"assessment_id":       asmtID,
		"drifted":             drifted,
		"succeeded":           succeeded,
		"last_assessment_at":  createdAt,
		"last_assessed_human": lastAssessedHuman,
		"assessment_status":   assessmentStatus,
		"summary":             summary,
		"resources_drifted":   resourcesDrifted,
		"resources_undrifted": resourcesUndrifted,
		"drifted_resources":   driftedResources,
		"drifted_addresses":   driftedAddresses,
		"error_message":       errMsg,
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

// fetchCurrentAssessment GETs the workspace's current assessment result.
// Returns (nil, nil) when the API responds with 404 — that means health
// assessments are not enabled for the workspace.
func fetchCurrentAssessment(ctx context.Context, token, wsID string, timeoutSec int) (map[string]any, *ToolError) {
	url := fmt.Sprintf("https://app.terraform.io/api/v2/workspaces/%s/current-assessment-result", wsID)
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &ToolError{ErrorCode: "execution_error", Message: "build assessment request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &ToolError{ErrorCode: "execution_error", Message: "fetch assessment: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, &ToolError{
			ErrorCode: "execution_error",
			Message:   fmt.Sprintf("assessment endpoint returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)),
			Retryable: resp.StatusCode >= 500,
		}
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, &ToolError{ErrorCode: "execution_error", Message: "read assessment body: " + rerr.Error()}
	}
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, &ToolError{ErrorCode: "marshal_error", Message: "decode assessment: " + err.Error()}
	}
	return doc.Data, nil
}

// driftedResource describes a single resource that has drifted from its
// recorded state, enriched with the inferred provider short-name and a
// human-readable change type.
type driftedResource struct {
	Address    string `json:"address"`
	Provider   string `json:"provider"`
	ChangeType string `json:"change_type"`
}

// fetchDriftedResources pulls the assessment's json-output (a Terraform plan
// JSON) and returns one entry per resource whose change.actions is anything
// other than ["no-op"]. Best-effort: returns an empty slice if the fetch or
// parse fails — the caller still reports the high-level drift counts.
func fetchDriftedResources(ctx context.Context, token, jsonOutPath string, timeoutSec int) []driftedResource {
	url := "https://app.terraform.io" + jsonOutPath
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return []driftedResource{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "tfpilot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []driftedResource{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return []driftedResource{}
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return []driftedResource{}
	}
	var plan struct {
		ResourceChanges []struct {
			Address      string `json:"address"`
			ProviderName string `json:"provider_name"`
			Change       struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		return []driftedResource{}
	}
	out := []driftedResource{}
	for _, rc := range plan.ResourceChanges {
		if !isDriftAction(rc.Change.Actions) || rc.Address == "" {
			continue
		}
		out = append(out, driftedResource{
			Address:    rc.Address,
			Provider:   providerFromAddress(rc.Address, rc.ProviderName),
			ChangeType: changeTypeFromActions(rc.Change.Actions),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// providerFromAddress derives the short provider name (e.g. "aws") from a
// Terraform resource address. Falls back to the plan's provider_name field
// when the address shape is ambiguous.
func providerFromAddress(address, providerName string) string {
	if i := strings.IndexAny(address, "_."); i > 0 {
		return address[:i]
	}
	if providerName != "" {
		if slash := strings.LastIndexByte(providerName, '/'); slash >= 0 && slash+1 < len(providerName) {
			return providerName[slash+1:]
		}
		return providerName
	}
	return ""
}

// changeTypeFromActions maps a Terraform change.actions list to a single
// human-readable change verb.
func changeTypeFromActions(actions []string) string {
	if len(actions) == 1 {
		switch actions[0] {
		case "update":
			return "modified"
		case "delete":
			return "deleted"
		case "create":
			return "added"
		case "no-op":
			return "no-op"
		}
	}
	if len(actions) == 2 && actions[0] == "delete" && actions[1] == "create" {
		return "replaced"
	}
	return "changed"
}

// buildDriftSummary assembles the plain-English summary string returned to
// the agent. Lives next to the payload assembly so the wording stays close
// to the fields it reads from.
func buildDriftSummary(status string, drifted bool, resourcesDrifted, resourcesUndrifted int, lastAssessedHuman, errMsg string) string {
	when := lastAssessedHuman
	if when == "" {
		when = "recently"
	}
	if status == "error" {
		if errMsg != "" {
			return fmt.Sprintf("Assessment failed (%s) — last attempted %s.", truncate(errMsg, 120), when)
		}
		return fmt.Sprintf("Assessment did not succeed, last attempted %s.", when)
	}
	if !drifted {
		if resourcesUndrifted > 0 {
			return fmt.Sprintf("No drift — %d resources in sync, last checked %s.", resourcesUndrifted, when)
		}
		return fmt.Sprintf("No drift detected, last checked %s.", when)
	}
	return fmt.Sprintf("%d resource(s) drifted, %d in sync, last checked %s.", resourcesDrifted, resourcesUndrifted, when)
}

func isDriftAction(actions []string) bool {
	if len(actions) == 0 {
		return false
	}
	for _, a := range actions {
		if a != "no-op" {
			return true
		}
	}
	return false
}

func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
