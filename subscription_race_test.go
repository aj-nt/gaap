package gaap

import (
	"context"
	"errors"
	"testing"
	"time"

	gstatus "github.com/aj-nt/gaap/internal/status"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestSubscriptionIsSingleWriter is the regression test for the DAG race: the
// stream reader goroutine and the orchestrator's loop must not touch task nodes
// concurrently. Run under -race (the suite always is); before the reader was
// reduced to handing events over, this reported a write from the reader's
// goroutine against the loop's DAG reads.
func TestSubscriptionIsSingleWriter(t *testing.T) {
	daemon, cleanup := setupDaemonScripted(t,
		&pb.TaskEntry{Id: "root", Status: "dead_letter"},
		&pb.TaskEntry{Id: "child-a", Status: "dead_letter"},
		&pb.TaskEntry{Id: "child-b", Status: "dead_letter"},
		&pb.TaskEntry{Id: "child-c", Status: "dead_letter"},
		&pb.TaskEntry{Id: "child-d", Status: "dead_letter"},
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	o := NewOrchestrator(ctx, &Config{RepoPath: "/tmp/x", MaxWaitSec: 15, PollIntervalSec: 1}, daemon, nil)
	o.goal = "single writer"
	o.state = &WaitingState{}
	o.subscribeFallbackToPoll = true

	// A subgraph with a dead-lettered root: the failure cascade writes many
	// nodes, which is exactly the mutation that must not race the loop.
	_ = o.dag.AddTask(&TaskNode{ID: "root", Status: gstatus.TaskClaimed, AgentType: "static_analysis"})
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		_ = o.dag.AddTask(&TaskNode{ID: "child-" + name, ParentIDs: []string{"root"}, Status: gstatus.TaskBlocked})
	}

	// Observation starts on its own goroutine; the loop applies what it posts.
	o.startWatcher()
	if err := o.drive(); err != nil && !errors.Is(err, ErrOrchestrationFailed) {
		t.Fatalf("drive: %v", err)
	}

	if got := o.dag.nodes["root"].Status; got != gstatus.TaskFailed {
		t.Fatalf("root = %s, want failed", got)
	}
	for _, node := range o.dag.nodes {
		if node.ID == "root" {
			continue
		}
		if node.Status != gstatus.TaskSkipped && node.Status != gstatus.TaskFailed {
			t.Fatalf("%s = %s, want skipped or failed", node.ID, node.Status)
		}
	}
	if !o.dag.AllTasksTerminal() {
		t.Fatal("DAG must be terminal after the only branch failed")
	}
}

// TestSubscriptionRequiresEvents keeps the fallback honest: a stream that closes
// without ever delivering a task event is a failed subscription, not a success.
func TestSubscriptionRequiresEvents(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "") // emits no tasks at all
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	o := NewOrchestrator(ctx, &Config{RepoPath: "/tmp/x", MaxWaitSec: 3, PollIntervalSec: 1}, daemon, nil)
	o.goal = "empty stream"
	o.state = &WaitingState{}
	o.subscribeFallbackToPoll = true
	// Work in flight, so the DAG is not terminal and the observer must actually
	// wait on the stream rather than returning on the first check.
	_ = o.dag.AddTask(&TaskNode{ID: "pending", Status: gstatus.TaskClaimed})

	o.startWatcher()

	// A stream that closes without ever delivering a task event is not a working
	// subscription: the watcher must fall back to polling rather than wait on
	// silence.
	deadline := time.Now().Add(5 * time.Second)
	for o.pollFallbacks.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("an empty stream did not fall back to polling")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunWithSubscriptionFailsOnDeadLetter drives the whole pipeline through the
// subscription path and checks the run still reports failure rather than success.
func TestRunWithSubscriptionFailsOnDeadLetter(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "will-fail")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	o := NewOrchestrator(ctx, &Config{RepoPath: "/tmp/x", MaxWaitSec: 15, PollIntervalSec: 1}, daemon,
		NewDecomposer(&StaticDecomposition{Tasks: []TaskSpec{
			{TaskID: "will-fail", AgentType: "flaky_agent", Status: "ready", Goal: "Dies"},
			{TaskID: "dependant", AgentType: "static_analysis", Status: "blocked", Goal: "Never runs", ParentIDs: []string{"will-fail"}},
		}}))
	o.subscribeFallbackToPoll = true

	err := o.Run("subscription failure")
	if !errors.Is(err, ErrOrchestrationFailed) {
		t.Fatalf("Run err = %v, want ErrOrchestrationFailed", err)
	}
	if got := nodeEndingIn(t, o.dag, "will-fail").Status; got != gstatus.TaskFailed {
		t.Fatalf("will-fail = %s, want failed", got)
	}
	if got := nodeEndingIn(t, o.dag, "dependant").Status; got != gstatus.TaskSkipped {
		t.Fatalf("dependant = %s, want skipped", got)
	}
}
