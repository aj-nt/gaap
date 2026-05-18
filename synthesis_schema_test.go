package gaap

import (
	"context"
	"testing"
)

func TestSchemaSynthesizeEmptyResults(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	result, err := s.Synthesize(context.Background(), map[string]*TaskResult{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Title != "Schema Cross-Reference (no results)" {
		t.Errorf("title = %q, want 'Schema Cross-Reference (no results)'", result.Title)
	}
	if len(result.Recommendations) == 0 {
		t.Error("expected recommendations in empty result")
	}
}

func TestSchemaSynthesizeDetectsNonZeroExitCode(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"exit_code": float64(1),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 1 {
		t.Fatalf("expected 1 high finding, got %d: %v", len(result.HighFindings), result.HighFindings)
	}
	if result.HighFindings[0].Title != "Tool exited with code 1" {
		t.Errorf("unexpected title: %q", result.HighFindings[0].Title)
	}
}

func TestSchemaSynthesizeDetectsBarePanics(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "quality_scan",
			Status:    "done",
			Findings: map[string]any{
				"bare_panics": float64(3),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 1 {
		t.Fatalf("expected 1 high finding, got %d", len(result.HighFindings))
	}
	f := result.HighFindings[0]
	if f.Title != "3 bare panic() calls detected" {
		t.Errorf("title = %q", f.Title)
	}
}

func TestSchemaSynthesizeDetectsLargeFiles(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "quality_scan",
			Status:    "done",
			Findings: map[string]any{
				"large_files": []any{"orchestrator.go", "states.go"},
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 2 {
		t.Fatalf("expected 2 high findings for 2 large files, got %d", len(result.HighFindings))
	}
}

func TestSchemaSynthesizeDetectsGodObjects(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "quality_scan",
			Status:    "done",
			Findings: map[string]any{
				"god_objects": float64(2),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 1 {
		t.Fatalf("expected 1 high finding, got %d", len(result.HighFindings))
	}
	if result.HighFindings[0].Title != "2 god objects detected" {
		t.Errorf("title = %q", result.HighFindings[0].Title)
	}
}

func TestSchemaSynthesizeDetectsHighCognitiveFunctions(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"high_cognitive_functions": float64(5),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 1 {
		t.Fatalf("expected 1 high finding, got %d", len(result.HighFindings))
	}
}

func TestSchemaSynthesizeDetectsUncoveredLines(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": {
			TaskID:    "t1",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"uncovered_lines": float64(42),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 1 {
		t.Fatalf("expected 1 high finding, got %d", len(result.HighFindings))
	}
}

func TestSchemaSynthesizeAccumulatesAcrossTasks(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"lint": {
			TaskID:    "lint",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"bare_panics":              float64(2),
				"high_cognitive_functions": float64(3),
			},
		},
		"qa": {
			TaskID:    "qa",
			AgentType: "quality_scan",
			Status:    "done",
			Findings: map[string]any{
				"god_objects": float64(1),
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	// 2 bare_panics + 3 high_cog + 1 god_object = 3 high findings
	if len(result.HighFindings) != 3 {
		t.Fatalf("expected 3 high findings across tasks, got %d", len(result.HighFindings))
	}
	// Verify executive summary mentions the count
	expected := "Found 3 high-severity items across 2 analysis results."
	if result.ExecutiveSummary != expected {
		t.Errorf("summary = %q, want %q", result.ExecutiveSummary, expected)
	}
}

func TestSchemaSynthesizeNilResultEntry(t *testing.T) {
	t.Parallel()
	s := &SchemaStrategy{}
	results := map[string]*TaskResult{
		"t1": nil,
		"t2": {
			TaskID:    "t2",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"exit_code": float64(0), // not flagged
			},
		},
	}

	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(result.HighFindings) != 0 {
		t.Errorf("expected 0 high findings (exit_code 0 not flagged), got %d", len(result.HighFindings))
	}
}
