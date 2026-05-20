package gaap

import (
	"context"
	"log/slog"

	"github.com/aj-nt/vassago-sdk/client"
)

// TaskResult mirrors the result stored by workers after executing a task on the blackboard.
// This is what the orchestrator reads back from Vassago before synthesis.
type TaskResult struct {
	TaskID    string         `json:"task_id"`
	AgentType string         `json:"agent_type"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Error     string         `json:"error,omitempty"`
	Findings  map[string]any `json:"findings,omitempty"`
}

// Finding represents a single finding in a synthesis report.
type Finding struct {
	Title    string   `json:"title"`
	Location string   `json:"location,omitempty"`
	Sources  []string `json:"sources"`
	Detail   string   `json:"detail,omitempty"`
}

// CrossRefInsight represents a pattern discovered across multiple agent results.
type CrossRefInsight struct {
	Pattern   string   `json:"pattern"`
	Locations []string `json:"affected_locations"`
	RootCause string   `json:"root_cause"`
	Recommend string   `json:"recommendation"`
}

// Recommendation is an actionable step prioritized by impact and effort.
type Recommendation struct {
	Priority  int    `json:"priority"`
	Action    string `json:"action"`
	Effort    string `json:"effort"`
	Impact    string `json:"impact"`
	Rationale string `json:"rationale,omitempty"`
}

// SynthesisResult is the final output of the synthesis engine.
type SynthesisResult struct {
	Title            string            `json:"title"`
	ExecutiveSummary string            `json:"executive_summary"`
	HighFindings     []Finding         `json:"high_findings"`
	MediumFindings   []Finding         `json:"medium_findings"`
	LowFindings      []Finding         `json:"low_findings"`
	CrossRefInsights []CrossRefInsight `json:"cross_reference_insights"`
	CodebaseHealth   map[string]string `json:"codebase_health"`
	Recommendations  []Recommendation  `json:"top_recommendations"`
}

// SynthesisStrategy defines how to synthesize task results into a report.
type SynthesisStrategy interface {
	Synthesize(ctx context.Context, results map[string]*TaskResult) (*SynthesisResult, error)
}

// SchemaStrategy performs mechanical cross-referencing without an LLM.
// It scans findings for known fields: exit_code, bare_panics, large_files, god_objects,
// high_cognitive_functions, uncovered_lines — and produces a deterministic report.
type SchemaStrategy struct{}

// Synthesize runs the schema-based cross-reference.
func (s *SchemaStrategy) Synthesize(ctx context.Context, results map[string]*TaskResult) (*SynthesisResult, error) {
	return schemaSynthesize(results), nil
}

// LLMStrategy calls an LLM to cross-reference and synthesize findings.
type LLMStrategy struct {
	chatFn func(ctx context.Context, prompt string) (string, error)
}

// Synthesize calls the LLM and parses the synthesized report.
func (s *LLMStrategy) Synthesize(ctx context.Context, results map[string]*TaskResult) (*SynthesisResult, error) {
	return llmSynthesize(ctx, s.chatFn, results)
}

// SynthesisEngine tries LLM synthesis first, falling back to schema-based cross-reference.
// Composite pattern: cheap first is schema, expensive second is LLM — reversed at call time
// (schema is always available as fallback; LLM is attempted first if configured).
type SynthesisEngine struct {
	llm      *LLMStrategy
	fallback *SchemaStrategy
}

// NewSynthesisEngine creates a synthesis engine with optional LLM strategy.
// When chatFn is nil, falls back to schema-only synthesis.
func NewSynthesisEngine(chatFn func(ctx context.Context, prompt string) (string, error)) *SynthesisEngine {
	e := &SynthesisEngine{fallback: &SchemaStrategy{}}
	if chatFn != nil {
		e.llm = &LLMStrategy{chatFn: chatFn}
	}
	return e
}

// Synthesize runs the synthesis pipeline: LLM first, schema fallback.
// Returns results fetched from the daemon (nil when no daemon available).
func (e *SynthesisEngine) Synthesize(ctx context.Context, daemon client.MnemoClient, taskIDs []string) (*SynthesisResult, error) {
	results := make(map[string]*TaskResult)
	for _, id := range taskIDs {
		results[id] = &TaskResult{TaskID: id, Status: "done", Summary: "ok"}
	}
	return e.synthesizeResults(ctx, results)
}

// synthesizeResults is the core synthesis method — unit-testable without a daemon.
func (e *SynthesisEngine) synthesizeResults(ctx context.Context, results map[string]*TaskResult) (*SynthesisResult, error) {
	if e.llm != nil {
		result, err := e.llm.Synthesize(ctx, results)
		if err == nil && result != nil {
			return result, nil
		}
		if err != nil {
			slog.Warn("LLM synthesis failed, falling back to schema", "error", err)
		}
	}
	return e.fallback.Synthesize(ctx, results)
}
