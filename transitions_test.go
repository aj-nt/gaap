package gaap

import (
	"context"
	"testing"
)

// TestLegalEdges pins the phase graph. Written out as data because the design
// doc draws this graph and, until now, nothing enforced it: Transition accepted
// whatever it was handed, so "done cannot go back to planning" was a sentence in
// a document rather than a property of the code.
func TestLegalEdges(t *testing.T) {
	cases := []struct {
		from, to Phase
		ok       bool
	}{
		{PhaseIdle, PhasePlanning, true},
		{PhaseIdle, PhaseWaiting, false},
		{PhaseIdle, PhaseDone, false},
		{PhasePlanning, PhaseWaiting, true},
		// A failed decomposition returns an error from Enter; the run ends
		// without a phase change. See Transition's caller in Run.
		{PhasePlanning, PhaseFailed, false},
		{PhaseWaiting, PhaseSynthesizing, true},
		{PhaseWaiting, PhaseFailed, true},
		{PhaseWaiting, PhaseApproving, true},
		{PhaseApproving, PhaseWaiting, true},
		{PhaseApproving, PhaseFailed, true},
		{PhaseSynthesizing, PhaseDone, true},
		{PhaseSynthesizing, PhaseWaiting, false},
		{PhaseDone, PhasePlanning, false},
		{PhaseFailed, PhaseWaiting, false},
		{PhaseFailed, PhaseDone, false},
		// Staying is expressed by returning nil from HandleEvent, not by
		// transitioning to the same phase.
		{PhaseWaiting, PhaseWaiting, false},
	}
	for _, c := range cases {
		if got := allowedTransition(c.from, c.to); got != c.ok {
			t.Errorf("allowedTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

// TestApprovingPhaseIsItsOwnName keeps the phase string stable: it is written to
// logs and to RunState, so a rename is a compatibility change, not a refactor.
func TestApprovingPhaseIsItsOwnName(t *testing.T) {
	seen := map[Phase]bool{}
	for _, p := range []Phase{
		PhaseIdle, PhasePlanning, PhaseWaiting, PhaseApproving,
		PhaseSynthesizing, PhaseDone, PhaseFailed,
	} {
		if p == "" {
			t.Fatal("empty phase in the vocabulary")
		}
		if seen[p] {
			t.Fatalf("phase %q is declared twice", p)
		}
		seen[p] = true
	}
	if PhaseApproving != "approving" {
		t.Fatalf("PhaseApproving = %q, want \"approving\" (it lands in RunState)", PhaseApproving)
	}
}

// TestTransitionRefusesIllegalEdge is the difference between a diagram and a
// machine: an edge the graph does not contain is refused, and the state does not
// move.
func TestTransitionRefusesIllegalEdge(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{}, nil, nil)
	o.state = &DoneState{}

	err := o.Transition(&PlanningState{})
	if err == nil {
		t.Fatal("done -> planning was accepted; the graph says it cannot happen")
	}
	if o.state.Phase() != PhaseDone {
		t.Fatalf("state moved to %q on a refused transition", o.state.Phase())
	}
}

// TestTransitionRefusesStaying: returning nil from HandleEvent means "stay".
// A transition to the current phase would hide a handler that meant to move but
// returned the wrong type.
func TestTransitionRefusesStaying(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{}, nil, nil)
	o.state = &WaitingState{}

	if err := o.Transition(&WaitingState{}); err == nil {
		t.Fatal("waiting -> waiting was accepted; staying is expressed by returning nil")
	}
}
