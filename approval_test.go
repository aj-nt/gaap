package gaap

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestApprovalDecisionValidActions: each of the four answers is valid when it
// carries what the code needs to act on it. A fifth answer does not exist,
// because an action the orchestrator cannot execute is not a decision.
func TestApprovalDecisionValidActions(t *testing.T) {
	t.Parallel()

	cases := []ApprovalDecision{
		{Action: ApprovalRetry, Reason: "the LLM was down, retry it"},
		{Action: ApprovalReassign, AgentType: "file_analysis", Reason: "wrong tool for the job"},
		{Action: ApprovalEdit, Context: `{"source_path":"/tmp/x.go"}`, Reason: "narrow the context"},
		{Action: ApprovalCancel, Reason: "not worth the tokens"},
	}
	for _, d := range cases {
		if err := d.Validate(); err != nil {
			t.Errorf("action %q should validate: %v", d.Action, err)
		}
	}
}

// TestApprovalDecisionRejects: every way a decision can be unusable.
func TestApprovalDecisionRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		d    ApprovalDecision
		want string // substring of the error
	}{
		{"unknown action", ApprovalDecision{Action: "maybe", Reason: "hm"}, "unknown action"},
		{"empty action", ApprovalDecision{Reason: "hm"}, "unknown action"},
		{"retry without reason", ApprovalDecision{Action: ApprovalRetry}, "reason"},
		{"blank reason", ApprovalDecision{Action: ApprovalCancel, Reason: "   "}, "reason"},
		{"reassign without type", ApprovalDecision{Action: ApprovalReassign, Reason: "r"}, "agent_type"},
		{"edit without context", ApprovalDecision{Action: ApprovalEdit, Reason: "r"}, "context"},
		{"edit with blank context", ApprovalDecision{Action: ApprovalEdit, Context: "  ", Reason: "r"}, "context"},
	}
	for _, c := range cases {
		err := c.d.Validate()
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q should mention %q", c.name, err, c.want)
		}
	}
}

// TestApprovalDecisionSurvivesJSON: the decision is written to a memory entry
// the orchestrator reads back, so the round trip is the real wire format.
func TestApprovalDecisionSurvivesJSON(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 11, 7, 4, 30, 0, 0, time.UTC)
	in := ApprovalDecision{
		Action:    ApprovalEdit,
		Context:   `{"source_path":"/tmp/x.go"}`,
		Reason:    "the goal named the wrong file",
		DecidedBy: "aj@studio",
		DecidedAt: when,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// The field names are the contract with `gaap approve`.
	for _, key := range []string{`"action"`, `"context"`, `"reason"`, `"decided_by"`, `"decided_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("marshalled decision should carry %s: %s", key, b)
		}
	}

	var out ApprovalDecision
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.DecidedAt.Equal(when) {
		t.Errorf("decided_at = %v, want %v", out.DecidedAt, when)
	}
	if out.Action != in.Action || out.Context != in.Context || out.Reason != in.Reason || out.DecidedBy != in.DecidedBy {
		t.Errorf("round trip lost fields: %+v", out)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("round-tripped decision should still validate: %v", err)
	}
}

// TestApprovalRequestDeadlineIsAbsolute: a request is opened with a timeout but
// stores a wall-clock deadline, so a restart cannot silently extend it.
func TestApprovalRequestDeadlineIsAbsolute(t *testing.T) {
	t.Parallel()

	opened := time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	req := NewApprovalRequest("af16_t1", "quality_scan", 2, "dead-lettered after 2 attempts", opened, time.Hour)

	if !req.OpenedAt.Equal(opened) {
		t.Errorf("opened_at = %v, want %v", req.OpenedAt, opened)
	}
	if !req.Deadline.Equal(opened.Add(time.Hour)) {
		t.Errorf("deadline = %v, want %v", req.Deadline, opened.Add(time.Hour))
	}
	if req.Expired(opened.Add(30 * time.Minute)) {
		t.Error("request should not be expired before its deadline")
	}
	if !req.Expired(opened.Add(time.Hour)) {
		t.Error("request should be expired at its deadline")
	}
}

// TestApprovalRequestWithoutTimeoutExpiresImmediately: a zero timeout is a
// configuration error, and the safe reading of it is "no time left", not
// "no deadline at all" — an unbounded wait turns a blocked run into a hung one.
func TestApprovalRequestWithoutTimeoutExpiresImmediately(t *testing.T) {
	t.Parallel()

	now := time.Now()
	req := NewApprovalRequest("t1", "coder", 1, "x", now, 0)
	if !req.Deadline.Equal(now) {
		t.Errorf("deadline = %v, want %v", req.Deadline, now)
	}
}
