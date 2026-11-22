package platform

import (
	"testing"
	"time"
)

// The milestone's claim is that the four classes are distinguishable. A table
// that produced three values, or that mapped "no artifact" onto "permanent",
// would pass a test written to check only a couple of cases.
func TestArtifactClassIsFourDistinctValues(t *testing.T) {
	cases := []struct {
		name string
		a    *Artifact
		want ArtifactClass
	}{
		{"nil", nil, ArtifactUnrecorded},
		{"bounded", &Artifact{Type: ArtifactCredential, Lifetime: time.Hour, Ceiling: ArtifactCeiling}, ArtifactBounded},
		{"over ceiling", &Artifact{Type: ArtifactCredential, Lifetime: 91 * 24 * time.Hour, Ceiling: ArtifactCeiling}, ArtifactOverCeiling},
		{"no lifetime", &Artifact{Type: ArtifactCredential, Ceiling: ArtifactCeiling}, ArtifactPermanent},
	}
	seen := map[ArtifactClass]bool{}
	for _, c := range cases {
		got := ArtifactClassOf(c.a)
		if got != c.want {
			t.Errorf("%s: class = %q, want %q", c.name, got, c.want)
		}
		seen[got] = true
	}
	if len(seen) != 4 {
		t.Fatalf("the four cases produced %d distinct classes, want 4: %v", len(seen), seen)
	}
}

// A loaded hook is not something an expiry clock reaches. A declared lifetime on
// one is a claim the platform cannot honour.
func TestSelfExtensionIsAlwaysPermanent(t *testing.T) {
	for _, typ := range []ArtifactType{ArtifactSkill, ArtifactHook, ArtifactMCP, ArtifactToolDef} {
		a := &Artifact{Type: typ, Lifetime: time.Minute, Ceiling: ArtifactCeiling}
		if got := ArtifactClassOf(a); got != ArtifactPermanent {
			t.Errorf("%s with a 1m lifetime classified %q, want permanent", typ, got)
		}
	}
}

func TestArtifactTypeKnown(t *testing.T) {
	for _, typ := range []ArtifactType{ArtifactCredential, ArtifactSkill, ArtifactDelete, ArtifactDisable} {
		if !typ.Known() {
			t.Errorf("%q should be known", typ)
		}
	}
	if ArtifactType("credentail").Known() {
		t.Error("a typo must not count as a known type")
	}
	if ArtifactType("").Known() {
		t.Error("empty must not count as a known type")
	}
}

// A lifetime with no ceiling in force is still bounded by its own lifetime —
// "nobody set a ceiling" must not read as "permanent".
func TestNoCeilingStillBounded(t *testing.T) {
	a := &Artifact{Type: ArtifactCredential, Lifetime: time.Hour}
	if got := ArtifactClassOf(a); got != ArtifactBounded {
		t.Fatalf("class = %q, want bounded", got)
	}
}

func TestCeilingValue(t *testing.T) {
	if ArtifactCeiling != 90*24*time.Hour {
		t.Fatalf("ceiling = %v, want 2160h", ArtifactCeiling)
	}
}
