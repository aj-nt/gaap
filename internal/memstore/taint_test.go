package memstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func appendExternal(t *testing.T, s *Store, id, body, producer string) {
	t.Helper()
	if err := s.Append(Entry{
		ID: id, Kind: "note", Body: body, SourceKind: TaintExternal,
		SourceRef: producer, Producer: producer, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func appendDerived(t *testing.T, s *Store, id, body string, parents ...string) {
	t.Helper()
	if err := s.Append(Entry{
		ID: id, Kind: "note", Body: body, SourceKind: TaintInternal,
		Parents: parents, Producer: "agent-7", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// The rephrasing test: a summary of an external observation is still external, however
// well it is written. taint_own says "internal" (it cites a parent) and taint_eff says
// "external" (something in the chain came from outside), and both are true statements
// about different things.
func TestRephrasingDoesNotLaunderTaint(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "e1", "a page said the cluster has 40 tenants", "scraper")
	appendDerived(t, s, "e2", "the cluster has 40 tenants", "e1")
	appendDerived(t, s, "e3", "roughly forty tenants, confirmed in my analysis", "e2")

	own, eff, err := s.EffectiveTaint("e3")
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if own != TaintInternal {
		t.Fatalf("own = %q, want internal (it cites a parent)", own)
	}
	if eff != TaintExternal {
		t.Fatalf("effective = %q, want external — two rephrasings laundered it", eff)
	}
}

func TestTicketedEntryIsInternalEffectively(t *testing.T) {
	s := open(t)
	if err := s.AppendWithTicket("tkt-1", Entry{
		ID: "e1", Kind: "note", Body: "IAM says this role has no policies",
		Producer: "broker", SourceRef: "iam:rolereport", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("append with ticket: %v", err)
	}
	own, eff, err := s.EffectiveTaint("e1")
	if err != nil {
		t.Fatal(err)
	}
	if own != TaintInternal || eff != TaintInternal {
		t.Fatalf("own=%q eff=%q, want internal/internal", own, eff)
	}
}

// A ticket is single use, and the constraint is a primary key rather than a flag.
func TestTicketIsSingleUse(t *testing.T) {
	s := open(t)
	e := Entry{ID: "e1", Kind: "note", Body: "x", Producer: "broker", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.AppendWithTicket("tkt-1", e); err != nil {
		t.Fatalf("first use: %v", err)
	}
	e.ID = "e2"
	if err := s.AppendWithTicket("tkt-1", e); !errors.Is(err, ErrTicketReused) {
		t.Fatalf("second use = %v, want ErrTicketReused", err)
	}
	// And the rejected append left nothing behind.
	if _, err := s.Get("e2"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a reused ticket still wrote the row: %v", err)
	}
}

func TestTicketedAppendRequiresProducer(t *testing.T) {
	s := open(t)
	if err := s.AppendWithTicket("tkt-1", Entry{
		ID: "e1", Kind: "note", Body: "x", ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("an internal append with no producer was accepted")
	}
}

// Promotion needs a principal that is not the producer: content vouching for itself is
// how a laundered claim becomes a fact.
func TestProducerCannotPromote(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "e1", "source A", "agent-7")
	appendDerived(t, s, "e2", "summary", "e1")
	if err := s.Promote("e2", "agent-7"); !errors.Is(err, ErrProducerCannotPromote) {
		t.Fatalf("self-promotion = %v, want ErrProducerCannotPromote", err)
	}
}

// Distinct ROOTS, not entry count: five summaries of one source are five entries and
// one origin, and counting entries lets a single source promote itself by being
// paraphrased twice.
func TestDistinctRootsNotEntryCount(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "root1", "one source", "scraper")
	appendDerived(t, s, "a", "paraphrase one", "root1")
	appendDerived(t, s, "b", "paraphrase two", "root1")
	appendDerived(t, s, "c", "summary of both paraphrases", "a", "b")

	if err := s.Promote("c", "operator:aj"); !errors.Is(err, ErrInsufficientRoots) {
		t.Fatalf("promotion of a single-origin chain = %v, want ErrInsufficientRoots", err)
	}

	// Add a genuinely independent second origin.
	appendExternal(t, s, "root2", "another source", "console")
	if err := s.Append(Entry{
		ID: "d", Kind: "note", Body: "agrees", SourceKind: TaintInternal,
		Parents: []string{"c", "root2"}, Producer: "agent-7", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	roots, err := s.distinctRoots("d")
	if err != nil {
		t.Fatal(err)
	}
	if roots != 2 {
		t.Fatalf("distinct roots = %d, want 2", roots)
	}
	if err := s.Promote("d", "operator:aj"); err != nil {
		t.Fatalf("promotion with two roots: %v", err)
	}
	_, eff, err := s.EffectiveTaint("d")
	if err != nil {
		t.Fatal(err)
	}
	if eff != TaintFact {
		t.Fatalf("effective = %q, want fact after promotion", eff)
	}
}

// Promotion is derived from an append-only row, so it cannot be walked back by
// rewriting a taint value.
func TestPromotionIsAppendOnly(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "root1", "a", "x")
	appendExternal(t, s, "root2", "b", "y")
	if err := s.Append(Entry{
		ID: "c", Kind: "note", Body: "both agree", SourceKind: TaintInternal,
		Parents: []string{"root1", "root2"}, Producer: "agent-7", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote("c", "operator:aj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE gov_promotion SET promoted_by = 'agent-7'`); err == nil {
		t.Fatal("a promotion row was rewritten")
	}
	if _, err := s.DB().Exec(`DELETE FROM gov_promotion`); err == nil {
		t.Fatal("a promotion row was deleted")
	}
	// And the entry's own taint is untouched.
	own, _, err := s.EffectiveTaint("c")
	if err != nil {
		t.Fatal(err)
	}
	if own != TaintInternal {
		t.Fatalf("own taint = %q, want the immutable internal it was written with", own)
	}
}

func TestPromotionRequiresPromoter(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "root1", "a", "x")
	if err := s.Promote("root1", ""); err == nil {
		t.Fatal("an anonymous promotion was accepted")
	}
}

// The closure walk is bounded; a store that follows edges without a limit is a store
// where one crafted write makes every read expensive.
func TestEffectiveTaintOnMissingEntry(t *testing.T) {
	s := open(t)
	if _, _, err := s.EffectiveTaint("nope"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

var _ = context.Background
