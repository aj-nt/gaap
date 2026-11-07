package gaap

import (
	"context"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/internal/status"

	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestRunNamesTheRun: the run id is the task namespace, so a human can find a
// run's tasks in the daemon by one string they were told at the start.
func TestRunNamesTheRun(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, autoComplete: true}
	client, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 5, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, client, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "coder", Status: status.TaskReady, Goal: "only"},
	})))

	if err := o.Run("name me"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runID := o.RunID()
	if len(runID) != 8 {
		t.Fatalf("run id = %q, want 8 hex characters", runID)
	}
	for _, node := range o.dag.nodes {
		if !strings.HasPrefix(node.ID, runID+"_") {
			t.Errorf("task id %q is not namespaced under the run id %q", node.ID, runID)
		}
	}
}

// TestDecomposeWithPrefixIsDeterministic: the caller's prefix wins. A dry run
// and the real run of the same goal then agree on ids, and a resumed run can
// find the tasks it left behind.
func TestDecomposeWithPrefixIsDeterministic(t *testing.T) {
	d := NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "a", AgentType: "coder", Status: status.TaskReady, Goal: "one"},
		{TaskID: "b", ParentIDs: []string{"a"}, AgentType: "coder", Status: status.TaskBlocked, Goal: "two"},
	}))

	first, err := d.DecomposeWithPrefix(context.Background(), "two levels", t.TempDir(), "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.DecomposeWithPrefix(context.Background(), "two levels", t.TempDir(), "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("got %d and %d tasks, want 2", len(first), len(second))
	}
	if first[0].TaskID != "abcd1234_a" {
		t.Errorf("task id = %q, want abcd1234_a", first[0].TaskID)
	}
	if first[1].TaskID != second[1].TaskID || first[1].ParentIDs[0] != first[0].TaskID {
		t.Errorf("prefix is not applied consistently: %+v vs %+v", first[1], second[1])
	}
}

// TestDecomposeWithoutPrefixStaysUnique: two runs of the same goal must not
// share ids when nobody supplied a namespace.
func TestDecomposeWithoutPrefixStaysUnique(t *testing.T) {
	d := NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "a", AgentType: "coder", Status: status.TaskReady, Goal: "one"},
	}))
	first, _ := d.Decompose(context.Background(), "one level", t.TempDir())
	second, _ := d.Decompose(context.Background(), "one level", t.TempDir())
	if first[0].TaskID == second[0].TaskID {
		t.Errorf("two runs produced the same task id %q", first[0].TaskID)
	}
}
