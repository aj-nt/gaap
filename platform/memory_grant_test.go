package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGrantFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const grantsJSON = `{
  "version": 1,
  "grants": [
    {"capability": "read",  "scope": "agent-7"},
    {"capability": "write", "scope": "agent-7", "max_ttl_seconds": 3600},
    {"capability": "resolve", "scope": "operator:aj"}
  ]
}`

// Deny-first: every capability is closed, and a grant opens exactly one.
func TestUngrantedCapabilityIsDenied(t *testing.T) {
	rules, _, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e := GrantsEngine(rules)

	if d := e.Evaluate("mem.read", map[string]any{"identity": "agent-7"}); d.Action != ActionAllow {
		t.Fatalf("granted read = %+v, want allow", d)
	}
	// Granted to someone else: the identity predicate is what stops this, and it is
	// a stable predicate — the caller cannot rename its way past it.
	if d := e.Evaluate("mem.read", map[string]any{"identity": "agent-8"}); d.Action != ActionDeny {
		t.Fatalf("read by an ungranted agent = %+v, want deny", d)
	}
	// A capability nobody granted.
	if d := e.Evaluate("mem.tombstone", map[string]any{"identity": "agent-7"}); d.Action != ActionDeny {
		t.Fatalf("tombstone = %+v, want deny", d)
	}
	// No identity at all cannot satisfy a predicate, so it must not match an allow.
	if d := e.Evaluate("mem.read", nil); d.Action != ActionDeny {
		t.Fatalf("anonymous read = %+v, want deny", d)
	}
}

// Every compiled rule names itself: a refusal that cannot say which rule produced it
// is the M6.1 defect.
func TestEveryRuleHasAStaticID(t *testing.T) {
	rules, _, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if strings.TrimSpace(r.ID) == "" {
			t.Fatalf("rule for %s has no id", r.Tool)
		}
	}
	// Derived ids are stable across loads, not sequence-dependent.
	rules2, _, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	for i := range rules {
		if rules[i].ID != rules2[i].ID {
			t.Fatalf("id %q is not stable across loads (got %q)", rules[i].ID, rules2[i].ID)
		}
	}
}

// A write grant carries its lifetime, and the lifetime goes through the same ceiling
// as every other artifact: memory does not get its own private rules.
func TestWriteGrantCarriesLifetimeWithinCeiling(t *testing.T) {
	rules, _, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	var write *Rule
	for i := range rules {
		if rules[i].Tool == "mem.write" && rules[i].Decision.Action == ActionAllow {
			write = &rules[i]
		}
	}
	if write == nil {
		t.Fatal("no write allow rule")
	}
	if write.Artifact == nil {
		t.Fatal("a write grant produced no artifact declaration")
	}
	if write.Artifact.Lifetime != time.Hour {
		t.Fatalf("lifetime = %v, want 1h", write.Artifact.Lifetime)
	}
}

func TestWriteGrantRequiresTTL(t *testing.T) {
	const bad = `{"version":1,"grants":[{"capability":"write","scope":"agent-7"}]}`
	if _, _, err := LoadMemoryGrants(writeGrantFile(t, bad)); err == nil {
		t.Fatal("a write grant with no lifetime was accepted; an entry with no expiry is permanent")
	}
}

func TestWriteGrantOverCeilingRefused(t *testing.T) {
	const bad = `{"version":1,"grants":[{"capability":"write","scope":"agent-7","max_ttl_seconds":31536000}]}`
	_, _, err := LoadMemoryGrants(writeGrantFile(t, bad))
	if err == nil {
		t.Fatal("a one-year memory lifetime was accepted")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("error does not name the ceiling: %v", err)
	}
}

func TestUnscopedGrantRefused(t *testing.T) {
	const bad = `{"version":1,"grants":[{"capability":"read","scope":""}]}`
	if _, _, err := LoadMemoryGrants(writeGrantFile(t, bad)); err == nil {
		t.Fatal("an unscoped grant was accepted")
	}
}

func TestUnknownCapabilityRefused(t *testing.T) {
	const bad = `{"version":1,"grants":[{"capability":"admin","scope":"agent-7"}]}`
	if _, _, err := LoadMemoryGrants(writeGrantFile(t, bad)); err == nil {
		t.Fatal("an unknown capability was accepted")
	}
}

// The revision is the file's content, so it changes when a grant does and does not
// when the format version string is left alone.
func TestRevisionIsContentHash(t *testing.T) {
	_, rev1, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	_, rev2, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if rev1 != rev2 {
		t.Fatal("the same file produced two revisions")
	}
	changed := strings.Replace(grantsJSON, `"agent-7"`, `"agent-9"`, 1)
	_, rev3, err := LoadMemoryGrants(writeGrantFile(t, changed))
	if err != nil {
		t.Fatal(err)
	}
	if rev3 == rev1 {
		t.Fatal("a changed grant produced the same revision")
	}
	if rev1 == "1" || len(rev1) != 64 {
		t.Fatalf("revision %q is not a sha256 hex digest", rev1)
	}
}

// Every declared capability must carry its own deny rule and its own wire name.
//
// Both are derived from allCapabilities, and this is what makes that safe: adding a
// capability to the slice without a deny rule used to be possible, and the consequence
// was a tool the engine refuses by fallthrough with an empty RuleID — a refusal nobody
// can attribute, which is the M6.1 defect the static ids were introduced to close.
func TestEveryDeclaredCapabilityIsClosed(t *testing.T) {
	rules, _, err := LoadMemoryGrants(writeGrantFile(t, grantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	denied := make(map[string]string, len(allCapabilities))
	for _, r := range rules {
		if r.Decision.Action == ActionDeny {
			denied[r.Tool] = r.ID
		}
	}
	for _, c := range allCapabilities {
		tool := MemoryTool(c)
		id, ok := denied[tool]
		if !ok {
			t.Errorf("capability %q (%s) has no deny rule; a caller that is not granted it "+
				"falls through with an unattributed refusal", c, tool)
			continue
		}
		if id == "" {
			t.Errorf("deny rule for %s has no id", tool)
		}
	}
}

// The wire names are spelled by grant files, policy files and the broker. Pinned so a
// rename cannot leave a grant that matches nothing while every test still passes.
func TestCapabilityWireNames(t *testing.T) {
	want := map[MemoryCapability]string{
		MemRead:       "mem.read",
		MemWrite:      "mem.write",
		MemResolve:    "mem.resolve",
		MemTombstone:  "mem.tombstone",
		MemInjectable: "mem.injectable",
	}
	if len(allCapabilities) != len(want) {
		t.Fatalf("allCapabilities has %d entries, the pinned vocabulary has %d: a capability "+
			"was added or removed without updating the wire contract", len(allCapabilities), len(want))
	}
	for c, wire := range want {
		if got := MemoryTool(c); got != wire {
			t.Errorf("MemoryTool(%q) = %q, want %q", c, got, wire)
		}
		if !c.known() {
			t.Errorf("capability %q is declared but not known()", c)
		}
	}
	if MemoryCapability("inject").known() {
		t.Error("an undeclared capability reports itself known")
	}
}
