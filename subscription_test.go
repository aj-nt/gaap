package gaap

import (
	"context"
	"testing"

	"github.com/aj-nt/vassago-sdk/client"
)

// TestSubscriptionFallsBackToPolling verifies that when a subscription fails
// (NullMnemo returns an EOF stream), the orchestrator falls back to polling
// and the pipeline still completes.
func TestSubscriptionFallsBackToPolling(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "t1", AgentType: "worker", Status: "ready", Goal: "task 1"},
		},
	})

	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, decomp)

	err := o.Run("test subscription fallback")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !o.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}
	if o.state == nil || o.state.Name() != "done" {
		t.Errorf("expected state done, got %q", o.state.Name())
	}
}

// TestSubscriptionPreference verifies that the orchestrator attempts
// subscription before falling back to polling. The NullMnemo's Subscribe()
// returns a stream that immediately yields io.EOF, so we detect the fallback.
func TestSubscriptionAttemptedBeforePolling(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "t1", AgentType: "worker", Status: "ready", Goal: "task 1"},
			{TaskID: "t2", AgentType: "worker", Status: "blocked", Goal: "task 2",
				ParentIDs: []string{"t1"}},
		},
	})

	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, decomp)
	o.subscribeFallbackToPoll = true

	err := o.Run("test sub preference")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !o.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}
}
