package gaap

// transitions is the legal edge set of the orchestrator's phase graph: the
// diagram in docs/design/orchestrator-patterns.md §2, plus the edges the human
// decision loop needs.
//
// An edge that is not in this table is a bug, not a style choice. Writing the
// graph down is what turns "done cannot go back to planning" from a sentence in
// a design doc into a property of the code.
//
// Note there is no planning -> failed edge: a failed decomposition returns an
// error from Enter, so the run ends without a phase change (the only
// Transition(&FailedState{}) call site is the timeout path, from waiting).
var transitions = map[Phase][]Phase{
	PhaseIdle:         {PhasePlanning},
	PhasePlanning:     {PhaseWaiting},
	PhaseWaiting:      {PhaseSynthesizing, PhaseFailed, PhaseApproving},
	PhaseApproving:    {PhaseWaiting, PhaseFailed},
	PhaseSynthesizing: {PhaseDone},
	PhaseDone:         nil,
	PhaseFailed:       nil,
}

// allowedTransition reports whether the graph permits from -> to. Staying put is
// expressed by returning nil from HandleEvent, so a self-transition is not an
// edge: it would hide a handler that meant to move and returned the wrong type.
func allowedTransition(from, to Phase) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}
