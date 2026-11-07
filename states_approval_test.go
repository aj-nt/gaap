package gaap

import (
	"encoding/json"
	"testing"
	"time"
)

// TestApprovalStateCarriesItsRequest is the whole reason the phase exists: it is
// the first state that owns data, and the data is what a restarted orchestrator
// needs to keep waiting for the same answer.
func TestApprovalStateCarriesItsRequest(t *testing.T) {
	deadline := time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	s := &ApprovalState{
		Request: ApprovalRequest{
			TaskID:    "t1",
			AgentType: "coder",
			Attempts:  2,
			Reason:    "dead-lettered after 2 attempts",
			Deadline:  deadline,
		},
		ApprovalTaskID: "af16f974_t1_approval",
		Deadline:       deadline,
	}

	if s.Phase() != PhaseApproving {
		t.Fatalf("phase = %q, want %q", s.Phase(), PhaseApproving)
	}
	if s.Request.TaskID != "t1" || s.ApprovalTaskID == "" {
		t.Fatalf("state does not hold its request: %+v", s)
	}
	if !s.Deadline.Equal(deadline) {
		t.Fatalf("deadline = %v, want %v", s.Deadline, deadline)
	}
}

// TestApprovalRequestSurvivesJSON: the request is persisted in RunState, so a
// round trip that loses the absolute deadline would silently restart the clock.
func TestApprovalRequestSurvivesJSON(t *testing.T) {
	deadline := time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	in := ApprovalRequest{
		TaskID:    "t1",
		AgentType: "coder",
		Attempts:  2,
		Reason:    "dead-lettered after 2 attempts",
		OpenedAt:  deadline.Add(-time.Hour),
		Deadline:  deadline,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ApprovalRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Deadline.Equal(in.Deadline) {
		t.Fatalf("deadline = %v, want %v", out.Deadline, in.Deadline)
	}
	if out.TaskID != in.TaskID || out.Attempts != in.Attempts || out.Reason != in.Reason {
		t.Fatalf("round trip lost fields: %+v", out)
	}
}
