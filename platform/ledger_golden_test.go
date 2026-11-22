package platform

import (
	"reflect"
	"testing"
	"time"
)

// goldenEntry is a fully specified entry from before the denial fields existed:
// the fields added later are left empty, which is exactly the shape of every row
// already on disk.
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

// goldenEntryWithDenial is the same row with the fields this milestone added.
func goldenEntryWithDenial() Entry {
	e := goldenEntry()
	e.DenialClass = "shell"
	e.CorrelationID = "0123456789abcdef"
	return e
}

const goldenPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// The chain hash is the only thing standing between an existing ledger and a
// silent rewrite: change the serialization and every previously written entry
// stops verifying. The other tests only prove self-consistency — they re-hash
// with the current function and compare, which is true even after a breaking
// change. This pins the actual digest, so regenerating it has to be deliberate.
//
// The value below predates M6's denial fields on purpose. If adding a field to
// Entry with `omitempty` ever changes this number, the change is not
// backwards-compatible and every ledger written before it stops verifying.
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

// The denial fields must change the hash when set — otherwise they are not in
// the chain and a tampered correlation id is undetectable.
func TestGoldenEntryHashCommitsToNewFields(t *testing.T) {
	plain, err := hashEntry(goldenEntry(), goldenPrevHash)
	if err != nil {
		t.Fatalf("hashEntry: %v", err)
	}
	withDenial, err := hashEntry(goldenEntryWithDenial(), goldenPrevHash)
	if err != nil {
		t.Fatalf("hashEntry: %v", err)
	}
	if plain == withDenial {
		t.Fatal("setting denial_class and correlation_id did not change the chain hash: the fields are outside the chain")
	}
	const want = "3e13a987aff9552c61c4af3d929fb5ef6515234d13522482555fa38691984e33"
	if withDenial != want {
		t.Fatalf("denial entry hash = %s, want %s", withDenial, want)
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
		"KeyID": true, "Algo": true, "DenialClass": true, "CorrelationID": true,
		"ArtifactType": true, "ArtifactClass": true, "ArtifactTTL": true,
		"ArtifactCeiling": true, "ArtifactScope": true, "ArtifactLocatable": true,
		"ArtifactPreState": true, "PolicyRev": true, "RunID": true, "ParentTasks": true,
		"ArtifactRef": true, "RevocationStage": true,
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
