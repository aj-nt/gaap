package gaap

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"

	"github.com/aj-nt/gaap/platform"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// governedConfig is a run with a governor attached and approval on, so a review
// has somewhere to go.
func governedConfig(g *platform.Governor) *Config {
	cfg := approvalConfig(10 * time.Minute)
	cfg.Governor = g
	return cfg
}

// policyFor builds a governor whose engine answers every tool with one decision.
// The real engine is exercised in platform's own tests; what these tests pin is
// what the orchestrator does with each answer.
func policyFor(t *testing.T, d platform.Decision) *platform.Governor {
	t.Helper()
	g := platform.NewGovernor()
	g.Policy.AddRule(platform.Rule{ID: "test-rule", Tool: "*", Decision: d})
	return g
}

// reviewFixture is a run with one ready task that the policy refers to a human,
// built the way a run is built: dispatch is what asks.
func reviewFixture(t *testing.T, cfg *Config) (*Orchestrator, *testDaemon) {
	t.Helper()
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	t.Cleanup(cleanup)

	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.runID = "abc12345"
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "abc12345_scan", Status: status.TaskReady, AgentType: "static_analysis", Goal: "scan"})
	o.state = &WaitingState{}
	_ = o.SaveRunState(context.Background(), "runstate_scan_abc12345")

	if err := o.dispatchTask(context.Background(), "abc12345_scan"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return o, td
}

// TestAGoverningDenyStopsTheTask: deny means the task never reaches the daemon.
// A policy that refuses a call and then lets it run is a suggestion.
func TestAGoverningDenyStopsTheTask(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := governedConfig(policyFor(t, platform.Decision{Action: platform.ActionDeny, Reason: "static analysis is not allowed here"}))
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.runID = "abc12345"
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "abc12345_scan", Status: status.TaskReady, AgentType: "static_analysis", Goal: "scan"})
	_ = o.dag.AddTask(&TaskNode{ID: "abc12345_report", Status: status.TaskBlocked, ParentIDs: []string{"abc12345_scan"}})

	if err := o.dispatchTask(context.Background(), "abc12345_scan"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if got := len(td.addedIDs()); got != 0 {
		t.Errorf("%d tasks reached the daemon, want 0: a denied task must never run", got)
	}
	node, _ := o.dag.GetTask("abc12345_scan")
	if node.Status != status.TaskFailed {
		t.Errorf("task status = %q, want failed", node.Status)
	}
	if !strings.Contains(node.Summary, "static analysis is not allowed here") {
		t.Errorf("summary = %q, want the policy's reason", node.Summary)
	}
	if child, _ := o.dag.GetTask("abc12345_report"); child.Status != status.TaskSkipped {
		t.Errorf("child status = %q, want skipped", child.Status)
	}
}

// TestAGoverningReviewAsksInsteadOfDispatching: review is the policy saying "a
// person should look at this", so the run asks instead of running it.
func TestAGoverningReviewAsksInsteadOfDispatching(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := governedConfig(policyFor(t, platform.Decision{Action: platform.ActionReview, Reason: "first run of this agent type"}))
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.runID = "abc12345"
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "abc12345_scan", Status: status.TaskReady, AgentType: "static_analysis", Goal: "scan"})

	if err := o.dispatchTask(context.Background(), "abc12345_scan"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	for _, id := range td.addedIDs() {
		if id == "abc12345_scan" {
			t.Error("a task under review was dispatched anyway")
		}
	}
	if len(o.pendingReviews) != 1 {
		t.Fatalf("pending reviews = %d, want 1", len(o.pendingReviews))
	}
	if o.pendingReviews[0].TaskID != "abc12345_scan" {
		t.Errorf("review is about %q", o.pendingReviews[0].TaskID)
	}
	if !strings.Contains(o.pendingReviews[0].Reason, "first run of this agent type") {
		t.Errorf("reason = %q, want the policy's", o.pendingReviews[0].Reason)
	}
}

// TestAReviewOpensAQuestion: the review queued at dispatch becomes a question a
// human can answer, with the policy's reason as the question.
func TestAReviewOpensAQuestion(t *testing.T) {
	o, _ := reviewFixture(t, governedConfig(policyFor(t, platform.Decision{Action: platform.ActionReview, Reason: "new agent type"})))

	// Entering the waiting phase has to announce the review: a held task is never
	// dispatched, so it produces no daemon event and nothing else would tell the
	// loop there is a question.
	if err := o.state.Enter(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	select {
	case evt := <-o.events:
		if err := o.step(evt); err != nil {
			t.Fatalf("stepping the queued review: %v", err)
		}
	default:
		t.Fatal("a queued review was never announced")
	}
	next, ok := o.state.(*ApprovalState)
	if !ok {
		t.Fatalf("state = %T, want *ApprovalState", o.state)
	}
	if next.Request.Kind != ApprovalKindReview {
		t.Errorf("kind = %q, want review", next.Request.Kind)
	}
	if !strings.Contains(next.Request.Reason, "new agent type") {
		t.Errorf("reason = %q, want the policy's", next.Request.Reason)
	}
}

// TestAnApprovedReviewDispatches: the human saying yes is what makes it run.
func TestAnApprovedReviewDispatches(t *testing.T) {
	o, td := reviewFixture(t, governedConfig(policyFor(t, platform.Decision{Action: platform.ActionReview, Reason: "new agent type"})))

	st := o.waitVerdict()
	review, ok := st.(*ApprovalState)
	if !ok {
		t.Fatalf("state = %T, want *ApprovalState", st)
	}
	if err := o.Transition(review); err != nil {
		t.Fatalf("opening the review question: %v", err)
	}

	next := answerApproval(t, o, review, ApprovalDecision{
		Action:    ApprovalRetry,
		Reason:    "run it, I want to see what it does",
		DecidedBy: "aj",
	})
	if err := o.Transition(next); err != nil {
		t.Fatalf("applying the answer: %v", err)
	}

	dispatched := false
	for _, id := range td.addedIDs() {
		if strings.HasPrefix(id, "abc12345_scan") && !strings.Contains(id, "approval") {
			dispatched = true
		}
	}
	if !dispatched {
		t.Errorf("nothing was dispatched after the review was approved; added=%v", td.addedIDs())
	}
}

// TestADeclinedReviewFailsTheTask: "no" is an answer, and the task does not run.
func TestADeclinedReviewFailsTheTask(t *testing.T) {
	o, td := reviewFixture(t, governedConfig(policyFor(t, platform.Decision{Action: platform.ActionReview, Reason: "new agent type"})))

	st := o.waitVerdict()
	review, ok := st.(*ApprovalState)
	if !ok {
		t.Fatalf("state = %T, want *ApprovalState", st)
	}
	if err := o.Transition(review); err != nil {
		t.Fatal(err)
	}

	next := answerApproval(t, o, review, ApprovalDecision{
		Action:    ApprovalCancel,
		Reason:    "not on this repo",
		DecidedBy: "aj",
	})
	_ = o.Transition(next)

	node, _ := o.dag.GetTask("abc12345_scan")
	if node.Status != status.TaskFailed {
		t.Errorf("task status = %q, want failed", node.Status)
	}
	if !strings.Contains(node.Summary, "not on this repo") {
		t.Errorf("summary = %q, want the human's reason", node.Summary)
	}
	for _, id := range td.addedIDs() {
		if id == "abc12345_scan" {
			t.Error("a declined task was dispatched anyway")
		}
	}
}

// TestGovernedDispatchRecordsADecisionPerTask: the ledger and the run are the
// same record, so every dispatch that was decided has an entry.
func TestGovernedDispatchRecordsADecisionPerTask(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, memories: map[string]*pb.MemoryEntry{}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	g := policyFor(t, platform.Allow)
	cfg := governedConfig(g)
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.runID = "abc12345"
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "abc12345_scan", Status: status.TaskReady, AgentType: "static_analysis", Goal: "scan"})

	if err := o.dispatchTask(context.Background(), "abc12345_scan"); err != nil {
		t.Fatal(err)
	}
	if got := len(g.Ledger.Entries()); got != 1 {
		t.Fatalf("ledger entries = %d, want 1 per dispatched task", got)
	}
	entry := g.Ledger.Entries()[0]
	if entry.Outcome != platform.ActionAllow {
		t.Errorf("outcome = %q, want allow", entry.Outcome)
	}
	if entry.Tool != "static_analysis" {
		t.Errorf("tool = %q, want the agent type the policy matched", entry.Tool)
	}
}
