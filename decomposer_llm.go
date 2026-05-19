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
func NewLLMDecomposition(chatFn func(ctx context.Context, prompt string) (string, error)) *LLMDecomposition {
	return &LLMDecomposition{
		chatFn:  chatFn,
		catalog: defaultAgentCatalog,
	}
}

// AgentSpec describes a worker agent's capabilities for the decomposer catalog.
type AgentSpec struct {
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Languages   []string `json:"languages"`
	Produces    string   `json:"produces"`
}

// defaultAgentCatalog is the hardcoded capability catalog for code review agents.
var defaultAgentCatalog = map[string]AgentSpec{
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
1. Leaf tasks (static_analysis, quality_scan) run in parallel. Set parent_ids=[] and status="ready".
2. The final task must be agent_type="synthesis" with parent_ids listing all leaf task_ids, status="blocked".
3. The synthesis task should NOT re-run analysis — it reads results from the blackboard.
4. context.repo_path must be the absolute path to the repository.
5. context.dependents on synthesis tasks must list the task_ids it depends on (for blackboard lookup).
6. Every task must produce results under a predictable key. Use context.output_key.

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
