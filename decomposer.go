package gaap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// TaskSpec represents a decomposed task to be dispatched to workers.
type TaskSpec struct {
	TaskID    string         `json:"task_id"`
	ParentIDs []string       `json:"parent_ids"`
	Status    string         `json:"status"`
	Goal      string         `json:"goal"`
	AgentType string         `json:"agent_type"`
	Context   map[string]any `json:"context"`
}

// DecomposerStrategy defines how to decompose a goal into tasks.
type DecomposerStrategy interface {
	Decompose(ctx context.Context, goal, repoPath string) ([]TaskSpec, error)
}

// Decomposer orchestrates the decomposition pipeline: validate → build prompt →
// call strategy → parse → validate DAG → prefix IDs. The pipeline steps are
// invariant (Template Method); the strategy call is pluggable (Strategy).
type Decomposer struct {
	strategy DecomposerStrategy
}

// NewDecomposer creates a decomposer with the given strategy.
func NewDecomposer(strategy DecomposerStrategy) *Decomposer {
	if strategy == nil {
		strategy = &StaticDecomposition{}
	}
	return &Decomposer{strategy: strategy}
}

// Decompose runs the full decomposition pipeline.
// If the goal references a specific file path, the pipeline short-circuits
// to a single file_analysis task — LLM-based decomposition is skipped.
func (d *Decomposer) Decompose(ctx context.Context, goal, repoPath string) ([]TaskSpec, error) {
	// Step 1: Validate input
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, fmt.Errorf("goal must not be empty")
	}

	// Step 1.5: Short-circuit file goals — no LLM decomposition needed.
	if filePath := extractFilePathFromGoal(goal); filePath != "" {
		return d.fileGoalTask(goal, filePath), nil
	}

	// Step 2: Build prompt (embedded in strategy call)
	// Step 3: Call strategy (LLM or static)
	tasks, err := d.strategy.Decompose(ctx, goal, repoPath)
	if err != nil {
		// Fallback to hardcoded decomposition on strategy failure
		return hardcodedDecompose(goal, repoPath, "fallback"), nil
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("decomposition produced no tasks")
	}

	// Step 4: Prefix task IDs to avoid collisions across orchestrator runs
	prefix := generatePrefix()
	idMap := make(map[string]string)
	for i := range tasks {
		newID := prefix + "_" + tasks[i].TaskID
		idMap[tasks[i].TaskID] = newID
		tasks[i].TaskID = newID
	}

	// Rewrite parent IDs to use prefixed versions
	for i := range tasks {
		newParents := make([]string, 0, len(tasks[i].ParentIDs))
		for _, pid := range tasks[i].ParentIDs {
			if newID, ok := idMap[pid]; ok {
				newParents = append(newParents, newID)
			}
		}
		tasks[i].ParentIDs = newParents
	}

	// Step 5: Validate DAG — check for cycles, ensure no self-references
	if err := validateTaskDAG(tasks); err != nil {
		return nil, fmt.Errorf("invalid DAG from strategy: %w", err)
	}

	// Inject repo path into context for tasks that don't have it
	for i := range tasks {
		if tasks[i].Context == nil {
			tasks[i].Context = make(map[string]any)
		}
		if _, ok := tasks[i].Context["source_path"]; !ok {
			tasks[i].Context["source_path"] = repoPath
		}
	}

	return tasks, nil
}

// validateTaskDAG checks a task list for structural validity.

// extractFilePathFromGoal scans a goal string for a file path.
// Returns the first path ending in a known file extension, or "" if none found.
func extractFilePathFromGoal(goal string) string {
	for ext := range fileExtensions {
		idx := strings.Index(goal, ext)
		if idx < 0 {
			continue
		}
		// Back up to find the start of the path (whitespace-bounded)
		end := idx + len(ext)
		start := idx
		for start > 0 && goal[start-1] != ' ' && goal[start-1] != '\t' && goal[start-1] != '"' && goal[start-1] != '\'' {
			start--
		}
		return goal[start:end]
	}
	return ""
}

// fileGoalTask returns a single file_analysis task for file-specific goals.
// The strategy is bypassed entirely — a file goal needs no decomposition.
func (d *Decomposer) fileGoalTask(goal, filePath string) []TaskSpec {
	prefix := generatePrefix()
	return []TaskSpec{
		{
			TaskID:    prefix + "_file_analysis",
			ParentIDs: nil,
			Status:    "ready",
			Goal:      goal,
			AgentType: "file_analysis",
			Context: map[string]any{
				"source_path": filePath,
			},
		},
	}
}

// fileExtensions is the set of extensions that indicate a file path rather
// than a directory in goals and context.source_path.
var fileExtensions = map[string]bool{
	".md": true, ".txt": true, ".go": true, ".py": true, ".js": true,
	".ts": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true,
	".cfg": true, ".ini": true, ".env": true, ".proto": true, ".rs": true,
	".c": true, ".h": true, ".cpp": true, ".java": true, ".rb": true,
	".csv": true, ".log": true, ".xml": true, ".html": true, ".css": true,
	".sql": true, ".sh": true, ".ps1": true, ".tf": true, ".hcl": true,
	".mod": true, ".sum": true,
}

func validateTaskDAG(tasks []TaskSpec) error {
	taskIDs := make(map[string]bool)
	for _, t := range tasks {
		taskIDs[t.TaskID] = true
	}

	for _, t := range tasks {
		for _, pid := range t.ParentIDs {
			if pid == t.TaskID {
				return fmt.Errorf("task %s has self-referencing parent", t.TaskID)
			}
			if !taskIDs[pid] {
				return fmt.Errorf("task %s references unknown parent %s", t.TaskID, pid)
			}
		}
	}

	return nil
}

// generatePrefix creates a short random prefix for task ID disambiguation.
func generatePrefix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StaticDecomposition returns a predefined set of tasks. Used for testing and
// as a deterministic baseline.
type StaticDecomposition struct {
	Tasks []TaskSpec
}

// NewStaticDecomposition creates a StaticDecomposition strategy from a
// predefined task list. Used in tests to define explicit DAGs.
func NewStaticDecomposition(tasks []TaskSpec) DecomposerStrategy {
	return &StaticDecomposition{Tasks: tasks}
}

func (s *StaticDecomposition) Decompose(ctx context.Context, goal, repoPath string) ([]TaskSpec, error) {
	// Deep copy to avoid mutation of the static list across calls.
	result := make([]TaskSpec, len(s.Tasks))
	for i, t := range s.Tasks {
		result[i] = t
		// Deep copy context map
		if t.Context != nil {
			result[i].Context = make(map[string]any, len(t.Context))
			for k, v := range t.Context {
				result[i].Context[k] = v
			}
		}
		// Deep copy parent IDs
		if t.ParentIDs != nil {
			result[i].ParentIDs = make([]string, len(t.ParentIDs))
			copy(result[i].ParentIDs, t.ParentIDs)
		}
	}
	return result, nil
}

// FailingDecomposition always returns an error. Used to test the fallback path.
type FailingDecomposition struct{}

func (f *FailingDecomposition) Decompose(ctx context.Context, goal, repoPath string) ([]TaskSpec, error) {
	return nil, fmt.Errorf("decomposition intentionally failed")
}

// hardcodedDecompose returns a fixed 3-task DAG as a fallback when the
// LLM strategy fails. Two leaf tasks + one synthesis task.
func hardcodedDecompose(goal, repoPath, reason string) []TaskSpec {
	leaf1ID := "static_analysis"
	leaf2ID := "quality_scan"
	synthID := "synthesis"

	return []TaskSpec{
		{
			TaskID:    leaf1ID,
			ParentIDs: nil,
			Status:    "ready",
			Goal:      fmt.Sprintf("Run static analysis (golangci-lint, gosec, govulncheck) on %s", repoPath),
			AgentType: "static_analysis",
			Context: map[string]any{
				"source_path": repoPath,
				"tools":       []string{"golangci-lint", "gosec", "govulncheck"},
			},
		},
		{
			TaskID:    leaf2ID,
			ParentIDs: nil,
			Status:    "ready",
			Goal:      fmt.Sprintf("Run code quality scan (god objects, long functions, cohesion) on %s", repoPath),
			AgentType: "quality_scan",
			Context: map[string]any{
				"source_path": repoPath,
			},
		},
		{
			TaskID:    synthID,
			ParentIDs: []string{leaf1ID, leaf2ID},
			Status:    "blocked",
			Goal:      fmt.Sprintf("Synthesize static analysis and quality scan results for %s into a unified audit report", repoPath),
			AgentType: "synthesis",
			Context: map[string]any{
				"source_path":     repoPath,
				"original_goal":   goal,
				"fallback_reason": reason,
				"dependents":      []string{leaf1ID, leaf2ID},
			},
		},
	}
}
