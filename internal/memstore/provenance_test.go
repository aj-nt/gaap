package memstore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// t2 hands a testing.TB to helpers that were written against *testing.T.
func t2(tb testing.TB) *testing.T {
	if t, ok := tb.(*testing.T); ok {
		return t
	}
	tb.Fatal("helper needs a *testing.T")
	return nil
}

// seedMixedProvenance builds one entry of every provenance shape the resolver has to get
// right, and returns the ids in a fixed order.
func seedMixedProvenance(tb testing.TB) (*Store, []string) {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "mem.db"))
	if err != nil {
		tb.Fatal(err)
	}
	live := time.Now().Add(time.Hour)
	ticketed := func(id string) {
		if err := s.AppendWithTicket("t-"+id, Entry{
			ID: id, Kind: "note", Body: id + " body", Producer: "broker",
			SourceRef: "/etc/" + id, ExpiresAt: live,
		}); err != nil {
			tb.Fatalf("ticket %s: %v", id, err)
		}
	}
	ticketed("r1")
	ticketed("r2")
	appendExternal(t2(tb), s, "ext", "a scraped page", "scraper")

	// One parent each: the chain's worst class is what a reader may rely on.
	if err := s.Append(Entry{ID: "derived-ext", Kind: "note", Body: "summary of a page",
		Parents: []string{"ext"}, ExpiresAt: live}); err != nil {
		tb.Fatal(err)
	}
	if err := s.Append(Entry{ID: "derived-int", Kind: "note", Body: "summary of a fact",
		Parents: []string{"r1"}, ExpiresAt: live}); err != nil {
		tb.Fatal(err)
	}
	// Two independent roots, vouched for: the one case a class may rise above its lineage.
	if err := s.Append(Entry{ID: "promoted", Kind: "note", Body: "two sources agree",
		Parents: []string{"r1", "r2"}, ExpiresAt: live}); err != nil {
		tb.Fatal(err)
	}
	if err := s.Promote("promoted", "operator"); err != nil {
		tb.Fatal(err)
	}
	// Reachable but past its expiry, and reachable but evicted: readable as history,
	// never injected.
	if err := s.AppendWithTicket("t-old", Entry{ID: "expired", Kind: "note",
		Body: "stale", Producer: "broker", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		tb.Fatal(err)
	}
	ticketed("evicted")
	if _, err := s.Evict([]string{"evicted"}); err != nil {
		tb.Fatal(err)
	}
	return s, []string{"r1", "ext", "derived-ext", "derived-int", "promoted", "expired", "evicted"}
}

// The eligibility rule, stated as literals rather than recomputed: Injectable must agree with
// the per-entry answer, and after the resolver is batched there is only one implementation
// left to be wrong — so the expectations are pinned here instead of derived from it.
func TestInjectableEligibilityAndClassesAreExact(t *testing.T) {
	s, ids := seedMixedProvenance(t)
	live := map[string]struct {
		own, effective string
	}{
		"r1":  {TaintInternal, TaintInternal},
		"ext": {TaintExternal, TaintExternal},
		// own is what the writer asserted (a derived row is internal: its provenance is
		// at least inspectable); effective is the worst class in the chain.
		"derived-ext": {TaintInternal, TaintExternal},
		"derived-int": {TaintInternal, TaintInternal},
		"promoted":    {TaintInternal, TaintFact},
		"expired":     {TaintInternal, TaintInternal},
		"evicted":     {TaintInternal, TaintInternal},
	}
	for _, id := range ids {
		own, eff, err := s.EffectiveTaint(id)
		if err != nil {
			t.Fatalf("EffectiveTaint(%s): %v", id, err)
		}
		want := live[id]
		if own != want.own || eff != want.effective {
			t.Errorf("%s: own=%s effective=%s, want own=%s effective=%s",
				id, own, eff, want.own, want.effective)
		}
	}
	if _, _, err := s.EffectiveTaint("nobody"); err == nil {
		t.Error("a missing entry resolved to a class instead of ErrUnreachable")
	}

	got, err := s.Injectable(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	injected := map[string]string{}
	for _, e := range got {
		injected[e.ID] = e.Effective
	}
	wantInjected := map[string]string{
		"r1": TaintInternal, "r2": TaintInternal,
		"derived-int": TaintInternal, "promoted": TaintFact,
	}
	if len(injected) != len(wantInjected) {
		t.Fatalf("injectable = %v, want exactly %v", injected, wantInjected)
	}
	for id, cls := range wantInjected {
		if injected[id] != cls {
			t.Errorf("injectable[%s] = %q, want %q", id, injected[id], cls)
		}
	}
}

// BenchmarkInjectable guards the defect this branch removes: resolving provenance walked the
// lineage once per candidate row, and did it before the cheap budget filter, so the cost of a
// prompt block grew with the size of memory rather than with what was in it.
func BenchmarkInjectable(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "mem.db"))
	if err != nil {
		b.Fatal(err)
	}
	live := time.Now().Add(time.Hour)
	for i := 0; i < 100; i++ {
		root := fmt.Sprintf("r%d", i)
		if err := s.AppendWithTicket("t-"+root, Entry{ID: root, Kind: "note",
			Body: "a fact worth remembering", Producer: "broker", ExpiresAt: live}); err != nil {
			b.Fatal(err)
		}
		// Two hops: the walk is the cost, one level would understate it.
		mid := fmt.Sprintf("m%d", i)
		if err := s.Append(Entry{ID: mid, Kind: "note", Body: "a summary",
			Parents: []string{root}, ExpiresAt: live}); err != nil {
			b.Fatal(err)
		}
		if err := s.Append(Entry{ID: fmt.Sprintf("d%d", i), Kind: "note", Body: "a summary of a summary",
			Parents: []string{mid}, ExpiresAt: live}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Injectable(4000); err != nil {
			b.Fatal(err)
		}
	}
}
