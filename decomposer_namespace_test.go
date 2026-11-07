package gaap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failingStrategy is a decomposition that always fails, so Decompose takes its
// hardcoded fallback path.
type failingStrategy struct{}

func (failingStrategy) Decompose(context.Context, string, string) ([]TaskSpec, error) {
	return nil, errors.New("model unavailable")
}

// TestDecomposeNamespacesEveryPath is the regression test for IDs that escaped
// Decompose unprefixed. The LLM-failure fallback returned before the namespacing
// step, so a degraded run dispatched static_analysis / quality_scan every time:
// the second run hit the daemon's UNIQUE constraint on tasks.id, and if the row
// already existed it silently adopted the previous run's status and result.
func TestDecomposeNamespacesEveryPath(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		strategy DecomposerStrategy
		goal     string
	}{
		{"static", NewStaticDecomposition([]TaskSpec{
			{TaskID: "a", AgentType: "coder", Status: "ready"},
			{TaskID: "b", ParentIDs: []string{"a"}, AgentType: "coder", Status: "blocked"},
		}), "audit this repo"},
		{"fallback", failingStrategy{}, "audit this repo"},
		{"file goal", NewStaticDecomposition(nil), "review the file main.go"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first, err := NewDecomposer(c.strategy).Decompose(ctx, c.goal, "/tmp/x")
			if err != nil {
				t.Fatalf("Decompose: %v", err)
			}
			if len(first) == 0 {
				t.Fatal("Decompose returned no tasks")
			}
			for _, task := range first {
				if !strings.Contains(task.TaskID, "_") {
					t.Fatalf("task ID %q is not namespaced", task.TaskID)
				}
			}

			// Every parent reference must point at a namespaced ID in the set,
			// or the DAG would have an edge to a task that does not exist.
			ids := map[string]bool{}
			for _, task := range first {
				ids[task.TaskID] = true
			}
			for _, task := range first {
				for _, pid := range task.ParentIDs {
					if !ids[pid] {
						t.Fatalf("task %s depends on unknown parent %s", task.TaskID, pid)
					}
				}
			}

			// Two runs must not share IDs.
			second, err := NewDecomposer(c.strategy).Decompose(ctx, c.goal, "/tmp/x")
			if err != nil {
				t.Fatalf("Decompose (second): %v", err)
			}
			for i := range first {
				if first[i].TaskID == second[i].TaskID {
					t.Fatalf("two runs produced the same task ID %q", first[i].TaskID)
				}
			}
		})
	}
}
