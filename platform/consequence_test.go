package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const consequencePolicy = `{
  "version": 1,
  "rules": [
    {"id": "issue-token", "tool": "CreateToken", "action": "allow",
     "reversibility": "irreversible",
     "artifact": {"type": "credential", "max_lifetime_seconds": 3600, "scope": "tenant"}},
    {"id": "issue-forever", "tool": "CreateLongToken", "action": "allow",
     "reversibility": "irreversible",
     "artifact": {"type": "credential", "max_lifetime_seconds": 0}},
    {"id": "issue-too-long", "tool": "CreateForeverToken", "action": "allow",
     "artifact": {"type": "credential", "max_lifetime_seconds": 15552000}},
    {"id": "install-hook", "tool": "InstallHook", "action": "allow",
     "artifact": {"type": "hook", "max_lifetime_seconds": 60}},
    {"id": "read", "tool": "ReadFile", "action": "allow", "reversibility": "pure"}
  ]
}`

func governorFromPolicy(t *testing.T, src string) *Governor {
	t.Helper()
	rules, err := ParsePolicy(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	g := NewGovernor()
	for _, r := range rules {
		g.Policy.AddRule(r)
	}
	return g
}

func TestRuleCarriesReversibility(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(consequencePolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if rules[0].Reversibility != Irreversible {
		t.Fatalf("rule reversibility = %q, want irreversible", rules[0].Reversibility)
	}
	d := NewPolicyEngine()
	for _, r := range rules {
		d.AddRule(r)
	}
	got := d.Evaluate("CreateToken", nil)
	if got.Reversibility != Irreversible {
		t.Fatalf("decision reversibility = %q, want the rule's declaration", got.Reversibility)
	}
	if got.Artifact == nil || got.Artifact.Type != ArtifactCredential {
		t.Fatalf("decision artifact = %+v, want a credential", got.Artifact)
	}
	if got.Artifact.Ceiling != ArtifactCeiling {
		t.Fatalf("artifact ceiling = %v, want %v", got.Artifact.Ceiling, ArtifactCeiling)
	}
}

// A rule that declares nothing falls back to the workload's classifier, and the
// kernel's default is the conservative one — never a guess at pure.
func TestUndeclaredRuleDefaultsIrreversible(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "any", Tool: "Mystery", Decision: Allow})
	d := g.Decide("agent-1", "Mystery", nil)
	e := g.Ledger.Entries()[0]
	if e.Reversibility != Irreversible {
		t.Fatalf("reversibility = %q, want irreversible", e.Reversibility)
	}
	if d.Action != ActionAllow {
		t.Fatalf("action = %q", d.Action)
	}
}

// The milestone's central claim: the cost of the call and the durability of its
// consequence are independent axes. A cheap call can leave something permanent,
// and a rule may declare either without implying the other.
func TestCallClassAndArtifactClassAreIndependent(t *testing.T) {
	g := governorFromPolicy(t, consequencePolicy)

	// bounded: 3600s declared, below the 90-day ceiling.
	g.Decide("agent-1", "CreateToken", nil)
	e := g.Ledger.Entries()[0]
	if e.Reversibility != Irreversible {
		t.Fatalf("reversibility = %q, want irreversible", e.Reversibility)
	}
	if e.ArtifactClass != string(ArtifactBounded) {
		t.Fatalf("artifact class = %q, want bounded", e.ArtifactClass)
	}
	if e.ArtifactType != "credential" || e.ArtifactScope != "tenant" {
		t.Fatalf("artifact = %q/%q, want credential/tenant", e.ArtifactType, e.ArtifactScope)
	}
	if e.ArtifactTTL != "1h0m0s" {
		t.Fatalf("ttl = %q, want 1h0m0s", e.ArtifactTTL)
	}

	// The mirror: same reversibility, no declared lifetime → permanent.
	g2 := governorFromPolicy(t, consequencePolicy)
	g2.Decide("agent-1", "CreateLongToken", nil)
	e2 := g2.Ledger.Entries()[0]
	if e2.Reversibility != Irreversible {
		t.Fatalf("reversibility = %q, want irreversible unchanged", e2.Reversibility)
	}
	if e2.ArtifactClass != string(ArtifactPermanent) {
		t.Fatalf("artifact class = %q, want permanent", e2.ArtifactClass)
	}
	if e2.ArtifactTTL != "" {
		t.Fatalf("ttl = %q, want empty for an unbounded artifact", e2.ArtifactTTL)
	}
}

// A call whose declaration outlives the ceiling is not allowed silently: it goes
// to review, and it stays attributable to the rule that produced it.
func TestOverCeilingAllowBecomesReview(t *testing.T) {
	g := governorFromPolicy(t, consequencePolicy)
	d := g.Decide("agent-1", "CreateForeverToken", nil)
	if d.Action != ActionReview {
		t.Fatalf("action = %q, want review", d.Action)
	}
	if d.RuleID != "issue-too-long" {
		t.Fatalf("rule id = %q, want issue-too-long preserved through the promotion", d.RuleID)
	}
	if !strings.Contains(d.Reason, "ceiling") {
		t.Fatalf("reason = %q, want it to name the ceiling", d.Reason)
	}
	e := g.Ledger.Entries()[0]
	if e.ArtifactClass != string(ArtifactOverCeiling) {
		t.Fatalf("artifact class = %q, want over_ceiling", e.ArtifactClass)
	}
}

// The other half: a lifetime inside the ceiling must not promote, or every
// bounded artifact becomes a human queue.
func TestWithinCeilingStaysAllow(t *testing.T) {
	g := governorFromPolicy(t, consequencePolicy)
	if d := g.Decide("agent-1", "CreateToken", nil); d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow", d.Action)
	}
}

// Self-extension is a durable capability, not a bounded one: a 60-second
// declared lifetime on a hook is a claim the platform cannot honour.
func TestSelfExtensionIsAlwaysIrreversibleAndPermanent(t *testing.T) {
	g := governorFromPolicy(t, consequencePolicy)
	g.Decide("agent-1", "InstallHook", nil)
	e := g.Ledger.Entries()[0]
	if e.Reversibility != Irreversible {
		t.Fatalf("reversibility = %q, want irreversible", e.Reversibility)
	}
	if e.ArtifactClass != string(ArtifactPermanent) {
		t.Fatalf("artifact class = %q, want permanent", e.ArtifactClass)
	}
}

// "pure" means no side effect at all, so a rule pairing it with a durable
// artifact is a lie. Refusing it at load is the only treatment that does not put
// the lie on the ledger.
func TestPureCannotCreateArtifact(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","reversibility":"pure","artifact":{"type":"credential"}}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("accepted pure + artifact")
	}
	if !strings.Contains(err.Error(), "pure") {
		t.Fatalf("error does not name the contradiction: %v", err)
	}
}

// The reserved classes exist as constants but must not be assignable: an inverse
// nobody holds is the unchecked-inverse error this kernel was built to avoid.
func TestReservedReversibilityRefused(t *testing.T) {
	for _, rev := range []string{"reversible", "compensable"} {
		p := `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","reversibility":"` + rev + `"}]}`
		if _, err := ParsePolicy(strings.NewReader(p)); err == nil {
			t.Errorf("accepted reserved reversibility %q", rev)
		}
	}
}

func TestUnknownArtifactTypeRefused(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","artifact":{"type":"credentail"}}]}`
	if _, err := ParsePolicy(strings.NewReader(p)); err == nil {
		t.Fatal("accepted a misspelled artifact type")
	}
}

// ---- M5.4: the fields that had no producer ----

func TestPolicyRevisionIsContentHash(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	// Same rules, different formatting.
	if err := os.WriteFile(a, []byte(consequencePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(strings.ReplaceAll(consequencePolicy, "\n", " ")), 0o600); err != nil {
		t.Fatal(err)
	}
	ea, err := LoadPolicyFile(a)
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	eb, err := LoadPolicyFile(b)
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	if ea.Revision() == "" {
		t.Fatal("loaded policy has no revision")
	}
	if ea.Revision() == "1" {
		t.Fatal("revision is the format version, not the content")
	}
	if len(ea.Revision()) != 64 {
		t.Fatalf("revision %q is not a sha256 hex digest", ea.Revision())
	}
	if ea.Revision() == eb.Revision() {
		t.Fatal("reformatting the file did not change the revision; the record would not name the instrument in force")
	}
	// And the revision reaches the ledger.
	g := NewGovernor()
	g.Policy = ea
	g.Decide("agent-1", "CreateToken", nil)
	if got := g.Ledger.Entries()[0].PolicyRev; got != ea.Revision() {
		t.Fatalf("ledger policy_rev = %q, want %q", got, ea.Revision())
	}
}

func TestHandBuiltEngineHasNoRevision(t *testing.T) {
	g := NewGovernor()
	if g.Policy.Revision() != "" {
		t.Fatalf("hand-built engine reports revision %q, want empty", g.Policy.Revision())
	}
	g.Policy.AddRule(Rule{ID: "any", Tool: "T", Decision: Allow})
	g.Decide("agent-1", "T", nil)
	if got := g.Ledger.Entries()[0].PolicyRev; got != "" {
		t.Fatalf("ledger policy_rev = %q, want empty", got)
	}
}

func TestRunIDAndParentsRecorded(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "any", Tool: "*", Decision: Allow})
	g.Decide("agent-1", "T", nil, WithRun("run-42"), WithParents("T1", "T2"))
	e := g.Ledger.Entries()[0]
	if e.RunID != "run-42" {
		t.Fatalf("run_id = %q", e.RunID)
	}
	if e.ParentTasks != "T1,T2" {
		t.Fatalf("parent_tasks = %q", e.ParentTasks)
	}
}

// The CLI path passes no options, and empty is the true answer there — not a
// placeholder that later reads as a run called "".
func TestNoOptionsRecordsEmptyLineage(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "any", Tool: "*", Decision: Allow})
	g.Decide("agent-1", "T", nil)
	e := g.Ledger.Entries()[0]
	if e.RunID != "" || e.ParentTasks != "" {
		t.Fatalf("lineage = %q/%q, want empty", e.RunID, e.ParentTasks)
	}
}

// Two halves of the same hazard. Empty new fields must hash exactly as before
// (the literal is pinned in ledger_golden_test.go); populated ones must change
// the digest, or a tampered artifact declaration is undetectable.
func TestOmitemptyFieldsPreserveHash(t *testing.T) {
	plain, err := hashEntry(goldenEntry(), goldenPrevHash)
	if err != nil {
		t.Fatalf("hashEntry: %v", err)
	}
	withArtifact := goldenEntry()
	withArtifact.ArtifactType = "credential"
	withArtifact.ArtifactClass = "bounded"
	withArtifact.ArtifactTTL = "1h0m0s"
	withArtifact.PolicyRev = "abc123"
	withArtifact.RunID = "run-42"
	populated, err := hashEntry(withArtifact, goldenPrevHash)
	if err != nil {
		t.Fatalf("hashEntry: %v", err)
	}
	if plain == populated {
		t.Fatal("populating the artifact fields did not change the digest: they are outside the chain")
	}
	if plain != "651a372a160640e0f89be451db492b161a7f1194d4acd06a3a24b3d6a06796b2" {
		t.Fatalf("empty new fields moved the hash of a pre-existing entry: %s", plain)
	}
}
