package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestGovernorKillSwitchWins(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "*", Decision: Allow})
	g.Kill.Trip("agent-7")

	d := g.Decide("agent-7", "Execute", map[string]any{"command": "ls"})
	if d.Action != ActionDeny {
		t.Fatalf("kill-switched call = %q, want deny", d.Action)
	}
	if d.Reason == "" {
		t.Fatal("kill-switch deny must carry a reason")
	}
}

func TestGovernorRecordsEveryDecision(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})
	g.Decide("agent-1", "ReadFile", nil)
	g.Decide("agent-1", "Execute", nil) // denied (no rule)

	if got := len(g.Ledger.Entries()); got != 2 {
		t.Fatalf("ledger len = %d, want 2 (one per decision)", got)
	}
}

func TestGovernorDenyPenalizesTrust(t *testing.T) {
	g := NewGovernor() // trust starts at 500
	before := g.Trust.Score()
	g.Decide("agent-1", "Execute", nil) // denied
	if got := g.Trust.Score(); got >= before {
		t.Fatalf("deny did not penalize trust: %v >= %v", got, before)
	}
}

// failingRing is a KeyRing whose Key always fails, used to prove a decision
// that cannot be committed is refused rather than recorded unkeyed.
type failingRing struct{}

func (failingRing) Key(string) ([]byte, error) { return nil, errRingUnavailable }
func (failingRing) Destroy(string) error       { return nil }
func (failingRing) Has(string) bool            { return false }

var errRingUnavailable = errors.New("key ring unavailable")

func TestGovernorRecordsKeyedCommitments(t *testing.T) {
	kr := NewMemoryKeyRing()
	c, err := NewCommitter(kr)
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	g := NewGovernor()
	g.Committer = c
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})

	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/tmp/x"})

	e := g.Ledger.Entries()[0]
	if e.CommitAlgo != CommitAlgoHMACSHA256 {
		t.Fatalf("CommitAlgo = %q, want %q", e.CommitAlgo, CommitAlgoHMACSHA256)
	}
	if e.SubjectRef == "" {
		t.Fatal("keyed decision did not record the subject ref; the commitment cannot be unlinked later")
	}
	sum := sha256.Sum256([]byte("agent-1"))
	if e.SubjectHash == hex.EncodeToString(sum[:]) {
		t.Fatal("subject commitment is a plain sha256 of the agent id; the ledger is a re-identification oracle")
	}
}

func TestGovernorErasureLeavesDecisionOnRecord(t *testing.T) {
	kr := NewMemoryKeyRing()
	c, _ := NewCommitter(kr)
	g := NewGovernor()
	g.Committer = c
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})

	g.Decide("agent-1", "ReadFile", nil)
	if err := kr.Destroy("agent-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	entries := g.Ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger len = %d, want the decision to survive the erasure", len(entries))
	}
	if entries[0].Tool != "ReadFile" || entries[0].Outcome != ActionAllow {
		t.Fatal("structural skeleton did not survive the erasure")
	}
	if err := g.Ledger.Verify(); err != nil {
		t.Fatalf("chain failed verification after the erasure: %v", err)
	}
	if ok, err := c.Match(entries[0].SubjectRef, "agent-1", entries[0].SubjectHash); ok {
		t.Fatalf("record still links to the erased subject (Match = %v, %v)", ok, err)
	}
	if g.Trust.Score() <= 0 {
		t.Fatal("trust manager broken by the erasure")
	}
}

func TestGovernorRefusesWhenCommitmentFails(t *testing.T) {
	c, err := NewCommitter(failingRing{})
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	g := NewGovernor()
	g.Committer = c
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})

	d := g.Decide("agent-1", "ReadFile", nil)
	if d.Action != ActionDeny {
		t.Fatalf("uncommittable decision = %q, want deny", d.Action)
	}
	if len(g.Ledger.Entries()) != 0 {
		t.Fatal("a decision that could not be committed was recorded anyway")
	}
}
