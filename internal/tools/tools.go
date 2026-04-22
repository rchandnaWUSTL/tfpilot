package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

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
	if name == "_hcp_tf_workspace_diff" {
		return workspaceDiffCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_workspace_describe" {
		return workspaceDescribeCall(ctx, args, timeoutSec)
	}
	if name == "_hcp_tf_variable_diff" {
		return variableDiffCall(ctx, args, timeoutSec)
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
			Description: "Returns a summary of a plan: adds/changes/destroys, flagged risks.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string", "description": "Run ID (run-xxx)"},
				},
				"required": []string{"run_id"},
			},
		},
	}
}

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}
