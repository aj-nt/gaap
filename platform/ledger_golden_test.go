package platform

import (
	"reflect"
	"testing"
	"time"
)

// goldenEntry is a fully specified entry: every hashed field is set to something
// distinctive, so any change to the serialization shows up as a different digest.
func goldenEntry() Entry {
	return Entry{
		Seq:           1,
		Time:          time.Unix(1700000000, 123456789).UTC(),
		Kind:          KindDecision,
		SubjectHash:   "subject-hash",
		SubjectRef:    "subject-ref",
		CommitAlgo:    "hmac-sha256",
		Tool:          "Execute",
		RuleID:        "rule-1",
		Outcome:       ActionDeny,
		InputHash:     "input-hash",
		Reason:        "no shell",
		Reversibility: Irreversible,
		KeyID:         "",
		Algo:          "sha256",
	}
}

const goldenPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// The chain hash is the only thing standing between an existing ledger and a
// silent rewrite: change the serialization and every previously written entry
// stops verifying. The other tests only prove self-consistency — they re-hash
// with the current function and compare, which is true even after a breaking
// change. This pins the actual digest, so regenerating it has to be deliberate.
func TestGoldenEntryHash(t *testing.T) {
	got, err := hashEntry(goldenEntry(), goldenPrevHash)
	if err != nil {
		t.Fatalf("hashEntry: %v", err)
	}
	const want = "651a372a160640e0f89be451db492b161a7f1194d4acd06a3a24b3d6a06796b2"
	if got != want {
		t.Fatalf("golden entry hash = %s, want %s\n"+
			"If this change is intentional it invalidates every existing ledger chain; "+
			"update the literal in this test and say so in the commit message.", got, want)
	}
}

// Entry and hashEntry's anonymous struct are two independent definitions of the
// same field set. Adding an Entry field without adding it to the chain means
// tampering with that field is undetectable — and nothing fails, because the
// entry still round-trips.
func TestEntryAndHashStructAgree(t *testing.T) {
	// Fields the chain commits to. Time is hashed as TimeUnixNano.
	hashed := map[string]bool{
		"Seq": true, "Time": true, "Kind": true, "SubjectHash": true,
		"SubjectRef": true, "CommitAlgo": true, "Tool": true, "RuleID": true,
		"Outcome": true, "InputHash": true, "Reason": true, "Reversibility": true,
		"KeyID": true, "Algo": true,
	}
	// Fields deliberately outside the chain, with the reason.
	notHashed := map[string]string{
		"Hash": "the output of hashEntry itself, not an input to it",
	}

	typ := reflect.TypeOf(Entry{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, ok := f.Tag.Lookup("json"); !ok {
			continue
		}
		if reason, ok := notHashed[f.Name]; ok {
			if hashed[f.Name] {
				t.Errorf("Entry.%s is listed both as hashed and as excluded (%s)", f.Name, reason)
			}
			continue
		}
		if !hashed[f.Name] {
			t.Errorf("Entry.%s has a json tag but hashEntry does not commit to it: "+
				"add it to the chain (and regenerate the golden hash) or record why it is excluded", f.Name)
		}
	}

	for name := range hashed {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("hashEntry commits to %q, which Entry no longer has", name)
		}
	}
}
