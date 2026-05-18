package gaap

import (
	"context"
	"testing"
)

// TestSyntheticComparison validates that the Go SchemaStrategy catches at least
// everything the Python spike's _basic_cross_reference() would have found,
// on a realistic multi-task resultset mirroring what workers produce.
func TestSyntheticComparison(t *testing.T) {
	t.Parallel()

	// Simulate results from a real 3-worker run on the vassago codebase:
	//   - lint worker: static_analysis (golangci-lint, go vet)
	//   - quality worker: quality_scan (file size, bare_panics, god_objects)
	//   - synthesis worker: synthesis (depends on both)
	results := map[string]*TaskResult{
		"lint-abc": {
			TaskID:    "lint-abc",
			AgentType: "static_analysis",
			Status:    "done",
			Summary:   "Found 12 lint issues including 2 bare panic calls",
			Findings: map[string]any{
				// golangci-lint exit_code 0 (clean — not flagged)
				"golangci-lint run ./...": map[string]any{
					"exit_code": float64(0),
					"stdout":    "internal/grpc/task_grpc.go:87:18: SA9003: empty branch",
				},
				// go vet exit_code 1 (flagged)
				"go vet ./...": map[string]any{
					"exit_code": float64(1),
					"stdout":    "composite literal uses unkeyed fields",
				},
			},
		},
		"qa-def": {
			TaskID:    "qa-def",
			AgentType: "quality_scan",
			Status:    "done",
			Summary:   "Identified 3 god objects, 4 large files, 5 uncovered lines",
			Findings: map[string]any{
				"go_count":                 float64(18),
				"bare_panics":              float64(2), // two bare panic() calls
				"god_objects":              float64(3), // three god objects
				"large_files":              []any{"orchestrator.go", "main.go", "cmd/watcher/main.go", "spike/worker_v5.py"},
				"high_cognitive_functions": float64(1), // one complex function
				"uncovered_lines":          float64(5),
				"coverage":                 "cmd/orchestrator/: 0%, internal/ollama/: 45%, daemon/internal/grpc/: 82%",
			},
		},
		"synthesis-ghi": {
			TaskID:    "synthesis-ghi",
			AgentType: "synthesis",
			Status:    "done",
			Summary:   "Synthesized findings from lint and quality scans",
			Findings: map[string]any{
				// synthesis tasks typically don't have the scanned metrics
				"cross_references": float64(3),
			},
		},
	}

	s := &SchemaStrategy{}
	result, err := s.Synthesize(context.Background(), results)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	// ── Assertions: every finding the Python spike would have caught must appear ──

	// The Python _basic_cross_reference scans for:
	//   - exit_code != 0
	//   - bare_panics > 0
	//   - large_files > 0 (per item)
	//
	// So it would find:
	//   1. "go vet" exit_code=1 (from lint-abc)
	//   2. 2 bare_panics (from qa-def)
	//   3. 4 large_files (from qa-def)
	//   = 6 high findings in Python's flattened model

	// Our Go SchemaStrategy adds: god_objects, high_cognitive_functions, uncovered_lines
	// So we should find MORE, not fewer.

	wantMinFindings := 6 // Python baseline
	if len(result.HighFindings) < wantMinFindings {
		t.Errorf("expected at least %d high findings (Python baseline), got %d: %v",
			wantMinFindings, len(result.HighFindings), result.HighFindings)
	}

	// Verify specific findings
	findingTitles := make(map[string]bool)
	for _, f := range result.HighFindings {
		findingTitles[f.Title] = true
	}

	checkTitle := func(t *testing.T, titles map[string]bool, title string) {
		t.Helper()
		if !titles[title] {
			t.Errorf("missing finding: %q", title)
		}
	}

	// Exit code finding
	checkTitle(t, findingTitles, "Tool exited with code 1")
	// Bare panics
	checkTitle(t, findingTitles, "2 bare panic() calls detected")
	// Large files (4 of them)
	largeCount := 0
	for _, f := range result.HighFindings {
		if len(f.Title) > 0 && f.Title[:11] == "Large file " {
			largeCount++
			// Verify source attribution
			if len(f.Sources) != 1 || f.Sources[0] != "qa-def" {
				t.Errorf("large file %q has wrong sources: %v", f.Title, f.Sources)
			}
		}
	}
	if largeCount != 4 {
		t.Errorf("expected 4 large file findings, got %d", largeCount)
	}
	// God objects (Go finds, Python spike didn't)
	checkTitle(t, findingTitles, "3 god objects detected")
	// High cognitive functions
	checkTitle(t, findingTitles, "1 high cognitive complexity functions")
	// Uncovered lines
	checkTitle(t, findingTitles, "5 uncovered lines")

	// Verify executive summary
	expectedSummary := "Found 9 high-severity items across 3 analysis results."
	if result.ExecutiveSummary != expectedSummary {
		t.Errorf("summary = %q, want %q", result.ExecutiveSummary, expectedSummary)
	}

	// Verify we have recommendations
	if len(result.Recommendations) == 0 {
		t.Error("expected at least 1 recommendation")
	}
}

// BenchmarkSchemaStrategy measures synthesis throughput on realistic task data.
func BenchmarkSchemaStrategy(b *testing.B) {
	results := map[string]*TaskResult{
		"lint": {
			TaskID:    "lint",
			AgentType: "static_analysis",
			Status:    "done",
			Findings: map[string]any{
				"golangci-lint": map[string]any{"exit_code": float64(0)},
				"go vet":        map[string]any{"exit_code": float64(1)},
			},
		},
		"qa": {
			TaskID:    "qa",
			AgentType: "quality_scan",
			Status:    "done",
			Findings: map[string]any{
				"bare_panics":              float64(2),
				"god_objects":              float64(3),
				"large_files":              []any{"a.go", "b.go", "c.go", "d.go"},
				"high_cognitive_functions": float64(1),
				"uncovered_lines":          float64(5),
			},
		},
	}

	s := &SchemaStrategy{}
	b.ResetTimer()
	for b.Loop() {
		_, _ = s.Synthesize(context.Background(), results)
	}
}
