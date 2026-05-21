package gaap

import (
	"context"
	"encoding/json"
	"fmt"
)

// LLMDecomposition is a DecomposerStrategy that calls an LLM to decompose goals
// into task DAGs. Falls back to returning an error (the Decomposer wrapper
// handles the actual hardcoded fallback).
type LLMDecomposition struct {
	chatFn  func(ctx context.Context, prompt string) (string, error)
	catalog map[string]AgentSpec
}

// NewLLMDecomposition creates an LLM-powered decomposition strategy.
// If catalog is nil, falls back to defaultAgentCatalog.
func NewLLMDecomposition(chatFn func(ctx context.Context, prompt string) (string, error), catalog map[string]AgentSpec) *LLMDecomposition {
	if catalog == nil {
		catalog = DefaultAgentCatalog
	}
	return &LLMDecomposition{
		chatFn:  chatFn,
		catalog: catalog,
	}
}

// AgentSpec describes a worker agent's capabilities for the decomposer catalog.
type AgentSpec struct {
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Languages   []string `json:"languages"`
	Produces    string   `json:"produces"`
}

// DefaultAgentCatalog is the built-in capability catalog for code review agents.
// Callers can extend or override it by passing a custom catalog to NewLLMDecomposition.
var DefaultAgentCatalog = map[string]AgentSpec{
	"static_analysis": {
		Description: "Runs static analysis tools: golangci-lint, gosec, govulncheck",
		Tools:       []string{"golangci-lint", "gosec", "govulncheck"},
		Languages:   []string{"go"},
		Produces:    "Report of lint issues, security vulnerabilities, and vulnerable dependencies",
	},
	"quality_scan": {
		Description: "Scans code quality: god objects, long functions, error handling, test coverage",
		Tools:       []string{"find", "grep", "go test -cover"},
		Languages:   []string{"go"},
		Produces:    "Report of code quality metrics: file sizes, function lengths, bare panics, test coverage",
	},
	"synthesis": {
		Description: "Synthesizes findings from multiple analysis agents into a unified audit",
		Tools:       []string{},
		Languages:   []string{},
		Produces:    "Cross-referenced synthesis report with prioritized findings and recommendations",
	},
	"file_analysis": {
		Description: "Reads and analyzes a specific file or set of files. First step is to read the target file directly. Do NOT discover the project first.",
		Tools:       []string{"cat", "head", "tail", "wc", "grep", "file", "stat"},
		Languages:   []string{},
		Produces:    "Summary, analysis, or extraction from the specified file(s)",
	},
}

var decomposePrompt = `You are an orchestrator that decomposes high-level goals into concrete, executable tasks for specialized AI agents.

Given a goal and the available agent types, produce a task DAG. Each task is a JSON object with:
- task_id: unique string (use "task_N" prefix)
- parent_ids: list of task_ids this depends on (empty for leaf tasks)
- status: "ready" for leaf tasks, "blocked" for tasks with dependencies
- goal: human-readable description of what this task should accomplish
- agent_type: one of the available types below
- context: a dict with repo_path and any tool-specific config the agent needs

AVAILABLE AGENT TYPES:
%s

RULES:
1. Detect the goal type FIRST. If the goal references a specific file path (ends in .md, .txt, .go, .py, .yaml, etc.), use file_analysis agent type. If the goal references a directory or project, use static_analysis + quality_scan + synthesis.
2. For file goals: one task, agent_type="file_analysis", context.source_path set to the exact file path.
3. For codebase goals: leaf tasks (static_analysis, quality_scan) run in parallel. Set parent_ids=[] and status="ready".
4. The final synthesis task must have agent_type="synthesis" with parent_ids listing all leaf task_ids, status="blocked".
5. The synthesis task should NOT re-run analysis — it reads results from the blackboard.
6. context.source_path must be the absolute path to the repository or target file.
7. context.dependents on synthesis tasks must list the task_ids it depends on (for blackboard lookup).
8. Every task must produce results under a predictable key. Use context.output_key.

GOAL: %s

Return ONLY a JSON array of task objects. No explanation, no markdown, no code fences.`

// Decompose calls the LLM and parses the resulting task DAG.
func (d *LLMDecomposition) Decompose(ctx context.Context, goal, repoPath string) ([]TaskSpec, error) {
	if d.chatFn == nil {
		return nil, fmt.Errorf("LLMDecomposition: chatFn is nil")
	}
	if goal == "" {
		return nil, fmt.Errorf("LLMDecomposition: goal is empty")
	}

	catalogJSON, err := json.MarshalIndent(d.catalog, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("LLMDecomposition: marshal catalog: %w", err)
	}

	prompt := fmt.Sprintf(decomposePrompt, string(catalogJSON), goal)

	raw, err := d.chatFn(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("LLMDecomposition: chat failed: %w", err)
	}

	raw = cleanJSONResponse(raw)

	var tasks []TaskSpec
	if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
		return nil, fmt.Errorf("LLMDecomposition: parse JSON: %w", err)
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("LLMDecomposition: produced 0 tasks")
	}

	// Validate agent types
	for _, t := range tasks {
		if t.TaskID == "" || t.AgentType == "" {
			return nil, fmt.Errorf("LLMDecomposition: task has empty ID or agent type")
		}
		if _, ok := d.catalog[t.AgentType]; t.AgentType != "synthesis" && !ok {
			return nil, fmt.Errorf("LLMDecomposition: unknown agent type %q for task %s", t.AgentType, t.TaskID)
		}
	}

	return tasks, nil
}
