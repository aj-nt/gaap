package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestGovernKillStopsLaterDecisions is the wiring that matters: the CLI builds a
// governor per invocation, so a kill switch that only lived in memory would be
// forgotten before the next decision was asked for.
func TestGovernKillStopsLaterDecisions(t *testing.T) {
	state, policy := governFixture(t)

	var killOut bytes.Buffer
	if err := governKill([]string{"--state", state, "--scope", "agent-7", "--reason", "incident-42"}, &killOut); err != nil {
		t.Fatalf("governKill: %v", err)
	}
	if !strings.Contains(killOut.String(), "agent-7") {
		t.Fatalf("kill output = %q, want the tripped scope", killOut.String())
	}

	out, err := runDecide(t, state, policy, "agent-7", "ReadFile", `{}`)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "deny") {
		t.Fatalf("output = %q, want a deny for a tripped scope", out)
	}
	if !strings.Contains(out, "agent-7") {
		t.Fatalf("output = %q, want the denial to name the tripped scope", out)
	}

	// A different agent is unaffected.
	out, err = runDecide(t, state, policy, "agent-8", "ReadFile", `{}`)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "allow") {
		t.Fatalf("output = %q, want an allow for an untripped agent", out)
	}
}

// TestGovernKillGlobalStopsEveryone checks the blast radius of the wide scope.
func TestGovernKillGlobalStopsEveryone(t *testing.T) {
	state, policy := governFixture(t)

	var killOut bytes.Buffer
	if err := governKill([]string{"--state", state, "--scope", "global"}, &killOut); err != nil {
		t.Fatalf("governKill: %v", err)
	}

	for _, subject := range []string{"agent-1", "agent-2"} {
		out, err := runDecide(t, state, policy, subject, "ReadFile", `{}`)
		if err != nil {
			t.Fatalf("governDecide(%s): %v", subject, err)
		}
		if !strings.Contains(out, "global") {
			t.Fatalf("output for %s = %q, want the scope named", subject, out)
		}
	}
}

// TestGovernKillRecordsTheTrip: a stop that is not on the record cannot be
// reconciled against the decisions it stopped.
func TestGovernKillRecordsTheTrip(t *testing.T) {
	state, _ := governFixture(t)

	if err := governKill([]string{"--state", state, "--scope", "agent-7", "--reason", "incident-42"}, new(bytes.Buffer)); err != nil {
		t.Fatalf("governKill: %v", err)
	}

	var status bytes.Buffer
	if err := governStatus([]string{"--state", state}, &status); err != nil {
		t.Fatalf("governStatus: %v", err)
	}
	got := status.String()
	if !strings.Contains(got, "agent-7") {
		t.Fatalf("status = %q, want the tripped scope", got)
	}
	if !strings.Contains(got, "entries:  1") {
		t.Fatalf("status = %q, want one recorded entry", got)
	}
}

func TestGovernKillRequiresScope(t *testing.T) {
	state, _ := governFixture(t)
	if err := governKill([]string{"--state", state}, new(bytes.Buffer)); err == nil {
		t.Fatal("governKill without --scope must fail")
	}
}

// TestGovernStatusOnEmptyState keeps the operator's first command honest: status
// on a directory that has never been used should report an empty state, not
// error out.
func TestGovernStatusOnEmptyState(t *testing.T) {
	state, _ := governFixture(t)

	var out bytes.Buffer
	if err := governStatus([]string{"--state", state}, &out); err != nil {
		t.Fatalf("governStatus: %v", err)
	}
	if !strings.Contains(out.String(), "tripped:  -") {
		t.Fatalf("status = %q, want an empty trip list", out.String())
	}
}

// TestGovernDecideReportsTrust pins the score being read on the way out: a
// score nobody can see is a score nobody can act on.
func TestGovernDecideReportsTrust(t *testing.T) {
	state, policy := governFixture(t)

	out, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{}`)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "trust:") {
		t.Fatalf("output = %q, want the trust score", out)
	}
}
