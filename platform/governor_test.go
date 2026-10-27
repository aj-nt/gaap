package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
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
func (failingRing) Destroyed(string) bool      { return false }

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

func TestGovernorKeysUnderRandomSubjectRefs(t *testing.T) {
	kr := NewMemoryKeyRing()
	c, _ := NewCommitter(kr)
	g := NewGovernor()
	g.Committer = c
	g.Refs = NewMemoryRefRegistry()
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})

	g.Decide("agent-1", "ReadFile", nil)

	e := g.Ledger.Entries()[0]
	if e.SubjectRef == "agent-1" {
		t.Fatal("commitments are keyed under the subject string; the ledger stores the identifier in the clear")
	}
	if !strings.HasPrefix(e.SubjectRef, "ref-") {
		t.Fatalf("SubjectRef = %q, want a minted handle", e.SubjectRef)
	}
}

// failingPutStore accepts nothing, standing in for a content store that is down.
type failingPutStore struct{}

func (failingPutStore) Put(string, string, []byte) error { return errStoreUnavailable }
func (failingPutStore) Get(string, string) ([]byte, error) {
	return nil, ErrContentNotFound
}
func (failingPutStore) Has(string) bool     { return false }
func (failingPutStore) Delete(string) error { return nil }

var errStoreUnavailable = errors.New("content store unavailable")

func newContentGovernor(t *testing.T) (*Governor, *MemoryContentStore) {
	t.Helper()
	c, err := NewCommitter(NewMemoryKeyRing())
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	g := NewGovernor()
	g.Committer = c
	content := NewMemoryContentStore()
	g.Content = content
	g.Policy.AddRule(Rule{ID: "reads", Tool: "ReadFile", Decision: Allow})
	return g, content
}

func TestGovernorStoresArgumentsAsErasableContent(t *testing.T) {
	g, content := newContentGovernor(t)
	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/etc/hosts"})

	e := g.Ledger.Entries()[0]
	got, err := content.Get(e.SubjectRef, e.InputHash)
	if err != nil {
		t.Fatalf("content Get by commitment: %v", err)
	}
	if string(got) != `{"path":"/etc/hosts"}` {
		t.Fatalf("stored content = %q, want the canonical arguments", got)
	}
	if bytes.Contains(got, []byte("agent-1")) {
		t.Fatal("stored content carries the subject identity")
	}
}

func TestGovernorKeepsContentPerDecisionNotPerSubject(t *testing.T) {
	g, content := newContentGovernor(t)
	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/a"})
	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/b"})

	entries := g.Ledger.Entries()
	first, err := content.Get(entries[0].SubjectRef, entries[0].InputHash)
	if err != nil {
		t.Fatalf("first decision content: %v", err)
	}
	second, err := content.Get(entries[1].SubjectRef, entries[1].InputHash)
	if err != nil {
		t.Fatalf("second decision content: %v", err)
	}
	if string(first) != `{"path":"/a"}` || string(second) != `{"path":"/b"}` {
		t.Fatalf("content = %q then %q, want each decision to keep its own arguments", first, second)
	}
}

func TestGovernorRefusesWhenContentCannotBeStored(t *testing.T) {
	c, _ := NewCommitter(NewMemoryKeyRing())
	g := NewGovernor()
	g.Committer = c
	g.Content = failingPutStore{}
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})

	d := g.Decide("agent-1", "ReadFile", map[string]any{"path": "/a"})
	if d.Action != ActionDeny {
		t.Fatalf("decision with unstorable content = %q, want deny", d.Action)
	}
	if got := len(g.Ledger.Entries()); got != 0 {
		t.Fatalf("ledger len = %d: a decision whose content was not stored was recorded with a commitment to nothing", got)
	}
}

func TestErasingTheSubjectRemovesStoredArguments(t *testing.T) {
	g, content := newContentGovernor(t)
	ring := NewMemoryKeyRing()
	committer, _ := NewCommitter(ring)
	g.Committer = committer
	g.Refs = NewMemoryRefRegistry()
	erasedRef, _ := g.Refs.Ref("agent-1")

	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/a"})
	g.Decide("agent-1", "ReadFile", map[string]any{"path": "/b"})
	if !content.Has(erasedRef) {
		t.Fatal("fixture: governor stored no content")
	}

	e := &Eraser{Content: content, Ring: ring, Ledger: g.Ledger, Refs: g.Refs}
	if err := e.EraseSubject("agent-1", "test"); err != nil {
		t.Fatalf("EraseSubject: %v", err)
	}

	if content.Has(erasedRef) {
		t.Fatal("arguments survived the erasure")
	}
	if err := g.Ledger.Verify(); err != nil {
		t.Fatalf("chain failed verification after erasure: %v", err)
	}
	if got := len(g.Ledger.Entries()); got != 3 {
		t.Fatalf("ledger len = %d, want 2 decisions plus the erase entry", got)
	}
}
