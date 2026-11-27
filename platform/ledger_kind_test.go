package platform

import "testing"

// The kinds are strings on disk. Renaming one silently splits the existing
// record: a tail filtered on "decision" stops matching rows written before the
// rename, and nothing reports it. These literals are therefore pinned.
func TestLedgerKindConstants(t *testing.T) {
	// Only the kinds a production path actually writes. Grant, trust, request and
	// revocation were declared and never emitted, so no row on disk carries them
	// and deleting the constants splits nothing; the phase that writes them
	// re-adds each with its writer.
	want := map[string]string{
		KindDecision:    "decision",
		KindKillswitch:  "killswitch",
		KindDenialBurst: "denial_burst",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("kind constant %q = %q, want %q (renaming splits the on-disk record)", expected, got, expected)
		}
	}

	seen := make(map[string]string, len(want))
	for got, name := range want {
		if prev, dup := seen[got]; dup {
			t.Errorf("kinds %s and %s are the same string %q", prev, name, got)
		}
		seen[got] = name
		if got == "" {
			t.Errorf("kind %s is empty", name)
		}
	}
}
