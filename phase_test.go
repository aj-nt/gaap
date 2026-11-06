package gaap

import "testing"

// TestPhaseVocabulary pins the phases and their values. The six names appear in
// logs and in the orchestrator's own phase gates; the gates are the reason this
// is typed at all — `o.state.Phase() == "waitng"` used to compile and silently
// skip a transition, turning a dead run into a nil return.
func TestPhaseVocabulary(t *testing.T) {
	states := []struct {
		state OrchestratorState
		want  Phase
	}{
		{&IdleState{}, PhaseIdle},
		{&PlanningState{}, PhasePlanning},
		{&WaitingState{}, PhaseWaiting},
		{&SynthesizingState{}, PhaseSynthesizing},
		{&DoneState{}, PhaseDone},
		{&FailedState{}, PhaseFailed},
	}
	for _, s := range states {
		if got := s.state.Phase(); got != s.want {
			t.Errorf("%T.Phase() = %q, want %q", s.state, got, s.want)
		}
	}

	// Values are readable strings, not iota: they land in logs.
	want := map[Phase]string{
		PhaseIdle:         "idle",
		PhasePlanning:     "planning",
		PhaseWaiting:      "waiting",
		PhaseSynthesizing: "synthesizing",
		PhaseDone:         "done",
		PhaseFailed:       "failed",
	}
	for phase, value := range want {
		if string(phase) != value {
			t.Errorf("phase %q = %q, want %q", phase, string(phase), value)
		}
	}
}

// TestPhaseGatesAreDistinct guards the thing the gates rely on: each phase
// compares unequal to the others, so a mixed-up constant cannot make two
// branches fire at once.
func TestPhaseGatesAreDistinct(t *testing.T) {
	all := []Phase{PhaseIdle, PhasePlanning, PhaseWaiting, PhaseSynthesizing, PhaseDone, PhaseFailed}
	seen := map[Phase]bool{}
	for _, p := range all {
		if seen[p] {
			t.Fatalf("duplicate phase value %q", p)
		}
		seen[p] = true
	}
}
