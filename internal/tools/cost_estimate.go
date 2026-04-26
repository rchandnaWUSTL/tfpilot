package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// costEstimate is the parsed payload returned by fetchCostEstimate. All fields
// are dollar amounts per month. DeltaSign is one of "increase", "decrease", or
// "no_change". Summary is a human-readable line the agent can surface verbatim.
type costEstimate struct {
	Prior     float64
	Proposed  float64
	Delta     float64
	DeltaSign string
	Summary   string
}

// fetchCostEstimate is a best-effort lookup of a run's HCP Terraform cost
// estimate. It returns nil whenever the estimate is unavailable for any reason
// — no token, no relationship, HTTP error, parse error, or the workspace has
// cost estimation disabled. Callers must treat nil as "no cost data" rather
// than as an error.
//
// Flow:
//  1. GET /api/v2/runs/<run-id>?include=cost-estimate to find the cost-estimate
//     relationship link.
//  2. GET that link to read prior/proposed/delta monthly cost.
//  3. Parse the string-encoded dollar amounts into floats and synthesize a
//     human-readable summary.
func fetchCostEstimate(ctx context.Context, runID string, timeoutSec int) *costEstimate {
	if runID == "" {
		return nil
	}
	token := readTFCToken()
	if token == "" {
		return nil
	}

	runURL := fmt.Sprintf("https://app.terraform.io/api/v2/runs/%s?include=cost-estimate", runID)
	runDoc, ok := tfcAPIGetJSON(ctx, token, runURL, timeoutSec)
	if !ok {
		return nil
	}

	related := costEstimateRelatedURL(runDoc)
	if related == "" {
		return nil
	}
	if !strings.HasPrefix(related, "http") {
		related = "https://app.terraform.io" + related
	}

	ceDoc, ok := tfcAPIGetJSON(ctx, token, related, timeoutSec)
	if !ok {
		return nil
	}

	data, _ := ceDoc["data"].(map[string]any)
	attrs, _ := data["attributes"].(map[string]any)
	if attrs == nil {
		return nil
	}
	prior, ok1 := parseStringAsFloat(attrs["prior-monthly-cost"])
	proposed, ok2 := parseStringAsFloat(attrs["proposed-monthly-cost"])
	delta, ok3 := parseStringAsFloat(attrs["delta-monthly-cost"])
	if !ok1 || !ok2 || !ok3 {
		return nil
	}

	sign := "no_change"
	switch {
	case delta > 0:
		sign = "increase"
	case delta < 0:
		sign = "decrease"
	}

	return &costEstimate{
		Prior:     prior,
		Proposed:  proposed,
		Delta:     delta,
		DeltaSign: sign,
		Summary:   buildCostSummary(prior, proposed, delta),
	}
}

// costEstimateRelatedURL pulls relationships.cost-estimate.links.related out of
// a run-read JSON:API document. Returns "" when any layer is missing.
func costEstimateRelatedURL(runDoc map[string]any) string {
	data, _ := runDoc["data"].(map[string]any)
	rels, _ := data["relationships"].(map[string]any)
	ce, _ := rels["cost-estimate"].(map[string]any)
	links, _ := ce["links"].(map[string]any)
	related, _ := links["related"].(string)
	return related
}

// buildCostSummary formats a cost delta the way the agent should surface it:
// "+$12.40/month (was $0.00, now $12.40)" for an increase, with a leading "-"
// for a decrease, and "No cost change estimated" when the delta is zero.
func buildCostSummary(prior, proposed, delta float64) string {
	if delta == 0 {
		return "No cost change estimated"
	}
	sign := "+"
	if delta < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s$%.2f/month (was $%.2f, now $%.2f)", sign, math.Abs(delta), prior, proposed)
}

// parseStringAsFloat coerces a JSON value (typically a string like "12.40", but
// also a raw number) into a float64. Returns ok=false when the value is missing
// or not a parseable number.
func parseStringAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		if x == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// tfcAPIGetJSON issues a GET against the HCP Terraform JSON:API and returns
// the decoded body. Any HTTP or parse failure (including HTML responses caught
// by htmlGuardError) collapses to ok=false so the caller can fall back to the
// "cost estimate unavailable" path without surfacing the underlying error.
func tfcAPIGetJSON(ctx context.Context, token, url string, timeoutSec int) (map[string]any, bool) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("User-Agent", "tfpilot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	if looksLikeHTML(string(body)) {
		return nil, false
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	return doc, true
}
