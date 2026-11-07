package gaap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"

	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestSavedStateCarriesThePendingApproval: a run that died while waiting for a
// human has to remember what it was waiting for, or resume restarts the wait
// from nothing and the question nobody can find is nobody's question.
func TestSavedStateCarriesThePendingApproval(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(10*time.Minute))
	o.runID = "abc12345"

	if err := o.SaveRunState(context.Background(), "run-abc12345"); err != nil {
		t.Fatalf("SaveRunState: %v", err)
	}

	var saved RunState
	if err := json.Unmarshal([]byte(td.memoryContent("run-abc12345")), &saved); err != nil {
		t.Fatalf("saved state is not a RunState: %v", err)
	}
	if saved.RunID != "abc12345" {
		t.Errorf("run id = %q, want abc12345", saved.RunID)
	}
	if saved.Pending == nil {
		t.Fatal("saved state has no pending approval")
	}
	if saved.Pending.ApprovalTaskID != s.ApprovalTaskID {
		t.Errorf("pending approval task = %q, want %q", saved.Pending.ApprovalTaskID, s.ApprovalTaskID)
	}
	if saved.Pending.Request.TaskID != "t1" {
		t.Errorf("pending request task = %q, want t1", saved.Pending.Request.TaskID)
	}
	if !saved.Pending.Deadline.Equal(s.Deadline) {
		t.Errorf("deadline = %v, want %v", saved.Pending.Deadline, s.Deadline)
	}
}

// TestResumeReEntersApproving: the question is still in the daemon, so resume
// adopts it instead of asking again.
func TestResumeReEntersApproving(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	req := ApprovalRequest{
		TaskID:    "abc12345_scan",
		AgentType: "quality_scan",
		Attempts:  2,
		Reason:    "dead-lettered",
		OpenedAt:  time.Now(),
		Deadline:  time.Now().Add(60 * time.Second),
	}
	// The daemon still holds the question the previous process created.
	_, _ = daemon.AddTask(context.Background(), "abc12345_scan_approval", ApprovalAgentType, "approve", "{}", 3, 600, 0)

	// The request's own deadline is a minute away, so the run's clock is what
	// ends this wait. Two seconds keeps the test honest about which clock fired.
	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 2}
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	rs := &RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks: []TaskSpec{
			{TaskID: "abc12345_scan", Status: status.TaskFailed, AgentType: "quality_scan", Goal: "inventory"},
		},
		Pending: &PendingApproval{Request: req, ApprovalTaskID: "abc12345_scan_approval", Deadline: req.Deadline},
	}

	start := time.Now()
	err := o.Resume(rs)
	if err == nil {
		t.Fatal("the run's own clock should have ended the resumed wait")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("resume waited %v; MaxWaitSec was 2s", elapsed)
	}

	phases := o.PhaseHistory()
	found := false
	for _, p := range phases {
		if p == PhaseApproving {
			found = true
		}
	}
	if !found {
		t.Fatalf("phase history = %v, want an approving phase", phases)
	}
}

// TestResumeReusesTheRequestAlreadyInTheDaemon: the restart adopts the question
// that is still waiting. Adding a second task under the same id would either
// fail the run on a duplicate id or split the answer in two.
func TestResumeReusesTheRequestAlreadyInTheDaemon(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	req := liveApprovalRequest()
	_, _ = daemon.AddTask(context.Background(), "abc12345_scan_approval", ApprovalAgentType, "approve", "{}", 3, 600, 0)
	before := len(td.addedIDs())

	o := NewOrchestrator(context.Background(), &Config{PollIntervalSec: 1, MaxWaitSec: 2}, daemon, nil)
	_ = o.Resume(&RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks:    []TaskSpec{{TaskID: "abc12345_scan", Status: status.TaskFailed, AgentType: "quality_scan", Goal: "inventory"}},
		Pending:  &PendingApproval{Request: req, ApprovalTaskID: "abc12345_scan_approval", Deadline: req.Deadline},
	})

	for _, id := range td.addedIDs()[before:] {
		if id == "abc12345_scan_approval" {
			t.Error("resume asked the same question twice")
		}
	}
}

// TestResumeReCreatesAMissingRequest: the daemon can lose the question — expired
// by its TTL, deleted, or restarted on a fresh database. A resumed run that
// assumes the question is still there waits for an answer nobody can give until
// its own deadline kills it. It should ask again, with the same id and deadline.
func TestResumeReCreatesAMissingRequest(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	req := liveApprovalRequest()
	o := NewOrchestrator(context.Background(), &Config{PollIntervalSec: 1, MaxWaitSec: 2}, daemon, nil)
	_ = o.Resume(&RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks:    []TaskSpec{{TaskID: "abc12345_scan", Status: status.TaskFailed, AgentType: "quality_scan", Goal: "inventory"}},
		Pending:  &PendingApproval{Request: req, ApprovalTaskID: "abc12345_scan_approval", Deadline: req.Deadline},
	})

	found := false
	for _, id := range td.addedIDs() {
		if id == "abc12345_scan_approval" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resume did not re-create the missing request; added=%v", td.addedIDs())
	}
}

// TestResumeReCreatesAFinishedRequest: a task already answered is not one that
// will be answered again. The watcher sees an update once; waiting on a task
// that is already done waits forever, so the run asks again instead.
func TestResumeReCreatesAFinishedRequest(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	req := liveApprovalRequest()
	_, _ = daemon.AddTask(context.Background(), "abc12345_scan_approval", ApprovalAgentType, "approve", "{}", 3, 600, 0)
	// Simulate a request that was answered while the orchestrator was down.
	td.mu.Lock()
	td.tasks["abc12345_scan_approval"].Status = "done"
	td.tasks["abc12345_scan_approval"].ResultKey = "mem-1"
	td.mu.Unlock()
	before := len(td.addedIDs())

	o := NewOrchestrator(context.Background(), &Config{PollIntervalSec: 1, MaxWaitSec: 2}, daemon, nil)
	_ = o.Resume(&RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks:    []TaskSpec{{TaskID: "abc12345_scan", Status: status.TaskFailed, AgentType: "quality_scan", Goal: "inventory"}},
		Pending:  &PendingApproval{Request: req, ApprovalTaskID: "abc12345_scan_approval", Deadline: req.Deadline},
	})

	found := false
	for _, id := range td.addedIDs()[before:] {
		if id == "abc12345_scan_approval" {
			found = true
		}
	}
	if !found {
		t.Errorf("a finished request was reused; added=%v", td.addedIDs()[before:])
	}
}

// liveApprovalRequest is a request whose deadline has not passed.
func liveApprovalRequest() ApprovalRequest {
	return ApprovalRequest{
		TaskID:    "abc12345_scan",
		AgentType: "quality_scan",
		Attempts:  2,
		Reason:    "dead-lettered",
		OpenedAt:  time.Now(),
		Deadline:  time.Now().Add(60 * time.Second),
	}
}

// TestResumeKeepsTheApprovalLoop: a run that asked humans before the crash must
// keep asking after the resume. The policy is part of the run, not of the
// invocation that resumed it.
func TestResumeKeepsTheApprovalLoop(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 2} // approval off in this process
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	rs := &RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks: []TaskSpec{
			{TaskID: "abc12345_scan", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"},
		},
		ApprovalPolicy: ApprovalPolicy{Enabled: true, Timeout: 10 * time.Minute, Agents: []string{"aj"}},
	}
	if err := o.Resume(rs); err != nil && !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("Resume: %v", err)
	}

	if !o.cfg.Approval.Enabled {
		t.Error("the resumed run forgot it was asking humans")
	}
	if o.cfg.Approval.Timeout != 10*time.Minute || len(o.cfg.Approval.Agents) != 1 {
		t.Errorf("policy = %+v, want the saved one", o.cfg.Approval)
	}
}

// TestResumeDoesNotExtendTheDeadline: a restart is not a reason to give the
// human more time. A request whose deadline has passed expires on resume.
func TestResumeDoesNotExtendTheDeadline(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	req := ApprovalRequest{
		TaskID:    "abc12345_scan",
		AgentType: "quality_scan",
		Attempts:  2,
		Reason:    "dead-lettered",
		OpenedAt:  time.Now().Add(-time.Hour),
		Deadline:  time.Now().Add(-time.Minute),
	}
	_, _ = daemon.AddTask(context.Background(), "abc12345_scan_approval", ApprovalAgentType, "approve", "{}", 3, 600, 0)

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 60}
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	rs := &RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks: []TaskSpec{
			{TaskID: "abc12345_scan", Status: status.TaskFailed, AgentType: "quality_scan", Goal: "inventory"},
		},
		Pending: &PendingApproval{Request: req, ApprovalTaskID: "abc12345_scan_approval", Deadline: req.Deadline},
	}

	start := time.Now()
	if err := o.Resume(rs); err == nil {
		t.Fatal("an expired request should fail the run on resume")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("resume waited %v for a deadline that had already passed", elapsed)
	}
}

// TestResumeKeepsTheDecomposersPrefix: a resumed run must not re-namespace the
// tasks it is about to poll for.
func TestResumeKeepsTheRunID(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, autoComplete: true}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	// A resumed run polls for the tasks it left behind; it does not re-dispatch
	// them, so the daemon still has to hold them.
	_, _ = daemon.AddTask(context.Background(), "abc12345_scan", "quality_scan", "inventory", "{}", 3, 300, 2)

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 5, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	rs := &RunState{
		Goal:     "inventory",
		RepoPath: t.TempDir(),
		RunID:    "abc12345",
		Tasks: []TaskSpec{
			{TaskID: "abc12345_scan", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"},
		},
	}
	if err := o.Resume(rs); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if o.RunID() != "abc12345" {
		t.Errorf("run id = %q, want abc12345", o.RunID())
	}
}
