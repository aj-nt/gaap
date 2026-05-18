package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// schemaSynthesize performs mechanical cross-referencing of task results.
// It scans for known finding fields without using an LLM.
func schemaSynthesize(results map[string]*TaskResult) *SynthesisResult {
	if len(results) == 0 {
		return &SynthesisResult{
			Title:            "Schema Cross-Reference (no results)",
			ExecutiveSummary: "No task results were available for synthesis.",
			Recommendations: []Recommendation{
				{Priority: 1, Action: "Verify workers completed their tasks", Effort: "low", Impact: "medium"},
			},
		}
	}

	var highFindings []Finding
	for tid, r := range results {
		if r == nil || r.Findings == nil {
			continue
		}
		f := r.Findings

		// Non-zero exit codes
		if ec, ok := f["exit_code"].(float64); ok && ec != 0 {
			highFindings = append(highFindings, Finding{
				Title:   fmt.Sprintf("Tool exited with code %.0f", ec),
				Sources: []string{tid},
				Detail:  fmt.Sprintf("exit_code: %.0f", ec),
			})
		}

		// Bare panics
		if bp, ok := f["bare_panics"].(float64); ok && bp > 0 {
			highFindings = append(highFindings, Finding{
				Title:   fmt.Sprintf("%.0f bare panic() calls detected", bp),
				Sources: []string{tid},
				Detail:  fmt.Sprintf("%.0f bare panic() calls — should use structured error handling", bp),
			})
		}

		// Large files
		if lf, ok := f["large_files"].([]any); ok && len(lf) > 0 {
			for _, item := range lf {
				if s, ok := item.(string); ok {
					highFindings = append(highFindings, Finding{
						Title:   fmt.Sprintf("Large file detected: %s", s),
						Sources: []string{tid},
						Detail:  fmt.Sprintf("File exceeds complexity threshold: %s", s),
					})
				}
			}
		}

		// God objects
		if gobj, ok := f["god_objects"].(float64); ok && gobj > 0 {
			highFindings = append(highFindings, Finding{
				Title:   fmt.Sprintf("%.0f god objects detected", gobj),
				Sources: []string{tid},
				Detail:  fmt.Sprintf("%.0f types with excessive responsibilities", gobj),
			})
		}

		// High cognitive complexity functions
		if hcf, ok := f["high_cognitive_functions"].(float64); ok && hcf > 0 {
			highFindings = append(highFindings, Finding{
				Title:   fmt.Sprintf("%.0f high cognitive complexity functions", hcf),
				Sources: []string{tid},
				Detail:  fmt.Sprintf("%.0f functions with complexity > 15", hcf),
			})
		}

		// Uncovered lines
		if ul, ok := f["uncovered_lines"].(float64); ok && ul > 0 {
			highFindings = append(highFindings, Finding{
				Title:   fmt.Sprintf("%.0f uncovered lines", ul),
				Sources: []string{tid},
				Detail:  fmt.Sprintf("%.0f lines without test coverage", ul),
			})
		}
	}

	return &SynthesisResult{
		Title:            "Schema Cross-Reference",
		ExecutiveSummary: fmt.Sprintf("Found %d high-severity items across %d analysis results.", len(highFindings), len(results)),
		HighFindings:     highFindings,
		Recommendations: []Recommendation{
			{Priority: 1, Action: "Address flagged items above", Effort: "medium", Impact: "high"},
		},
	}
}

// llmSynthesize calls an LLM via the provided chat function and parses the result.
func llmSynthesize(ctx context.Context, chatFn func(ctx context.Context, prompt string) (string, error), results map[string]*TaskResult) (*SynthesisResult, error) {
	if chatFn == nil {
		return nil, fmt.Errorf("LLM strategy requires a chat function")
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no results to synthesize")
	}

	// Build compact result summaries
	summaries := make(map[string]any)
	for tid, r := range results {
		summaries[tid] = map[string]any{
			"agent_type": r.AgentType,
			"summary":    r.Summary,
			"status":     r.Status,
			"findings":   r.Findings,
		}
	}

	resultsJSON, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal results for LLM: %w", err)
	}

	prompt := fmt.Sprintf(`You are a synthesis agent. Combine findings from code analysis agents into a report.

Below are results from %d agents. Cross-reference findings, prioritize by severity,
identify root causes, and produce recommendations.

FORMAT AS JSON:
{"title":"...","executive_summary":"2-3 sentences","high_findings":[{"title":"...","location":"...","sources":["id1"],"detail":"..."}],"medium_findings":[],"low_findings":[],"cross_reference_insights":[{"pattern":"...","affected_locations":[],"root_cause":"...","recommendation":"..."}],"codebase_health":{"overall_score":"N/100","security_score":"N/100","maintainability_score":"N/100","test_coverage_assessment":"..."},"top_recommendations":[{"priority":1,"action":"...","effort":"low|medium|high","impact":"low|medium|high","rationale":"..."}]}

ANALYSIS RESULTS:
%s
`, len(results), string(resultsJSON))

	raw, err := chatFn(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("LLM synthesis failed: %w", err)
	}

	// Clean common LLM output artifacts
	raw = cleanJSONResponse(raw)

	var result SynthesisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse synthesis JSON: %w (raw preview: %.100s)", err, raw)
	}

	return &result, nil
}

// cleanJSONResponse strips markdown fences, leading/trailing whitespace, and
// common LLM artifacts from a JSON response string.
func cleanJSONResponse(raw string) string {
	raw = strings.TrimSpace(raw)

	// Strip markdown code fences: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(raw, "```") {
		// Find the first newline after the opening fence
		idx := strings.Index(raw, "\n")
		if idx > 0 {
			raw = raw[idx+1:]
		}
		// Strip trailing fence
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
	}

	return strings.TrimSpace(raw)
}
