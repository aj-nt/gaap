package gaap

import (
	"strings"
	"testing"
)

// TestFormatSynthesisReport verifies the human-readable report output.
func TestFormatSynthesisReport(t *testing.T) {
	t.Parallel()

	sr := &SynthesisResult{
		Title:            "Test Audit Report",
		ExecutiveSummary: "Found 2 issues in test codebase.",
		HighFindings: []Finding{
			{Title: "Command injection risk", Location: "executor.go:173", Detail: "sh -c with LLM input", Sources: []string{"gosec"}},
		},
		MediumFindings: []Finding{
			{Title: "Unchecked error", Location: "orchestrator.go:241", Detail: "Transition error discarded", Sources: []string{"golangci-lint"}},
		},
		CrossRefInsights: []CrossRefInsight{
			{Pattern: "Error handling gap", RootCause: "Missing error checks in state transitions", Recommend: "Add structured error handling"},
		},
		CodebaseHealth: map[string]string{
			"test_coverage": "54 passing",
			"go_version":    "1.26.3",
		},
		Recommendations: []Recommendation{
			{Priority: 1, Action: "Add command allowlist", Effort: "small", Impact: "high", Rationale: "Fixes G204 CWE-78"},
		},
	}

	report := formatSynthesisReport(sr)

	checks := []string{
		"Test Audit Report",
		"Found 2 issues",
		"HIGH SEVERITY",
		"MEDIUM SEVERITY",
		"Command injection",
		"Unchecked error",
		"CROSS-REFERENCE",
		"Error handling gap",
		"CODEBASE HEALTH",
		"test_coverage",
		"go_version",
		"TOP RECOMMENDATIONS",
		"Add command allowlist",
		"Effort: small",
		"Impact: high",
	}

	for _, check := range checks {
		if !strings.Contains(report, check) {
			t.Errorf("report missing %q", check)
		}
	}

	// Verify LOW SEVERITY section is omitted (none present)
	if strings.Contains(report, "LOW SEVERITY") {
		t.Error("report should not have LOW SEVERITY section when no low findings")
	}
}

// TestSanitizeGoal verifies unsafe characters are stripped.
func TestSanitizeGoal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"test: colon, comma.", "test colon comma"},
		{"path/to/file.go", "pathtofilego"},
		{"mixed-CASE_123", "mixed-CASE_123"},
		{"very-long-goal-that-exceeds-forty-characters-total", "very-long-goal-that-exceeds-forty-charac"},
	}

	for _, tc := range tests {
		got := sanitizeGoal(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeGoal(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
