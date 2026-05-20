package gaap

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSynthesisEngineLLMSuccess(t *testing.T) {
	t.Parallel()

	// Mock chatFn returns valid JSON
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		result := &SynthesisResult{
			Title:            "LLM-Generated Report",
			ExecutiveSummary: "Everything is fine.",
			Recommendations: []Recommendation{
				{Priority: 1, Action: "Keep doing what you're doing", Effort: "low", Impact: "medium"},
			},
		}
		b, _ := json.Marshal(result)
		return string(b), nil
	}

	engine := NewSynthesisEngine(chatFn)
	results := map[string]*TaskResult{
		"t1": {TaskID: "t1", Status: "done", Summary: "ok"},
	}

	result, err := engine.synthesizeResults(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Title != "LLM-Generated Report" {
		t.Errorf("expected LLM title, got %q (engine fell through to schema)", result.Title)
	}
}

func TestSynthesisEngineLLMFailureFallsBackToSchema(t *testing.T) {
	t.Parallel()

	// Mock chatFn always errors
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return "", context.DeadlineExceeded
	}

	engine := NewSynthesisEngine(chatFn)
	results := map[string]*TaskResult{
		"t1": {
			TaskID:   "t1",
			Status:   "done",
			Findings: map[string]any{"exit_code": float64(1)},
		},
	}

	result, err := engine.synthesizeResults(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize (fallback): %v", err)
	}
	if result.Title != "Schema Cross-Reference" {
		t.Errorf("expected schema fallback title, got %q", result.Title)
	}
	if len(result.HighFindings) != 1 {
		t.Errorf("expected 1 high finding from schema fallback, got %d", len(result.HighFindings))
	}
}

func TestSynthesisEngineNilLLMUsesSchema(t *testing.T) {
	t.Parallel()

	// No LLM configured
	engine := NewSynthesisEngine(nil)
	results := map[string]*TaskResult{
		"t1": {
			TaskID:   "t1",
			Status:   "done",
			Findings: map[string]any{"bare_panics": float64(2)},
		},
	}

	result, err := engine.synthesizeResults(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Title != "Schema Cross-Reference" {
		t.Errorf("expected schema title, got %q", result.Title)
	}
	if len(result.HighFindings) != 1 {
		t.Errorf("expected 1 high finding, got %d", len(result.HighFindings))
	}
}

func TestSynthesisEngineEmptyResults(t *testing.T) {
	t.Parallel()

	engine := NewSynthesisEngine(nil)
	result, err := engine.synthesizeResults(context.Background(), map[string]*TaskResult{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Title != "Schema Cross-Reference (no results)" {
		t.Errorf("title = %q", result.Title)
	}
}

func TestLLMStrategyReturnsParsedResult(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		result := &SynthesisResult{
			Title:            "Test Report",
			ExecutiveSummary: "Summary here.",
			HighFindings: []Finding{
				{Title: "Critical bug", Sources: []string{"t1", "t2"}},
			},
			CrossRefInsights: []CrossRefInsight{
				{Pattern: "Shared root cause", RootCause: "Missing validation"},
			},
			CodebaseHealth: map[string]string{
				"overall_score": "75/100",
			},
			Recommendations: []Recommendation{
				{Priority: 1, Action: "Fix it", Effort: "medium", Impact: "high"},
			},
		}
		b, _ := json.Marshal(result)
		return string(b), nil
	}

	results := map[string]*TaskResult{
		"t1": {TaskID: "t1", Status: "done", Summary: "ok"},
	}

	strategy := &LLMStrategy{chatFn: chatFn}
	result, err := strategy.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Title != "Test Report" {
		t.Errorf("title = %q", result.Title)
	}
	if len(result.HighFindings) != 1 {
		t.Errorf("expected 1 high finding, got %d", len(result.HighFindings))
	}
	if len(result.Recommendations) != 1 {
		t.Errorf("expected 1 recommendation, got %d", len(result.Recommendations))
	}
}

func TestLLMStrategyCleansFencedJSON(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		// LLMs often wrap JSON in markdown fences
		result := &SynthesisResult{
			Title:            "Fenced Report",
			ExecutiveSummary: "Unwrapped.",
		}
		b, _ := json.Marshal(result)
		return "```json\n" + string(b) + "\n```", nil
	}

	results := map[string]*TaskResult{
		"t1": {TaskID: "t1", Status: "done", Summary: "ok"},
	}

	strategy := &LLMStrategy{chatFn: chatFn}
	result, err := strategy.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Title != "Fenced Report" {
		t.Errorf("title = %q, expected 'Fenced Report' (cleanJSONResponse should strip fences)", result.Title)
	}
}

func TestLLMStrategyEmptyResults(t *testing.T) {
	t.Parallel()

	strategy := &LLMStrategy{chatFn: func(ctx context.Context, prompt string) (string, error) {
		return "{}", nil
	}}
	_, err := strategy.Synthesize(context.Background(), map[string]*TaskResult{})
	if err == nil {
		t.Fatal("expected error for empty results")
	}
}

func TestCleanJSONResponseStripsFences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{`{"key": "value"}`, `{"key": "value"}`},
		{"```json\n{\"key\": \"value\"}\n```", `{"key": "value"}`},
		{"```\n{\"key\": \"value\"}\n```", `{"key": "value"}`},
		{"  {\"key\": \"value\"}  ", `{"key": "value"}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
	}

	for _, tc := range tests {
		got := cleanJSONResponse(tc.input)
		if got != tc.expected {
			t.Errorf("cleanJSONResponse(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}
