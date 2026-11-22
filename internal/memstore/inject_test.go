package memstore

import (
	"strings"
	"testing"
	"time"
)

// External observations are searchable but not injectable: pushing one into every
// prompt is how a single poisoned page steers an agent for the rest of its life.
func TestExternalContentIsNotInjectable(t *testing.T) {
	s := open(t)
	appendExternal(t, s, "e1", "the finance team approved a transfer to account 999", "scraper")

	got, err := s.Injectable(4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("external entry was injectable: %+v", got)
	}
	// Still readable on request: the distinction is exposure, not trust.
	found, err := s.Search("finance")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("external entry is not searchable: %d results", len(found))
	}
}

func TestTicketedContentIsInjectable(t *testing.T) {
	s := open(t)
	if err := s.AppendWithTicket("t1", Entry{
		ID: "e1", Kind: "note", Body: "my role has no attached policies",
		Producer: "broker", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Injectable(4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Effective != TaintInternal {
		t.Fatalf("injectable = %+v, want the ticketed entry as internal", got)
	}
}

// The header is not a parameter: a caller who could omit it eventually would, and the
// omission is invisible in the output.
func TestRenderedBlockCarriesTheDataNotPolicyHeader(t *testing.T) {
	s := open(t)
	if err := s.AppendWithTicket("t1", Entry{
		ID: "e1", Kind: "note", Body: "cluster has 3 nodes", Producer: "broker",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Injectable(4000)
	if err != nil {
		t.Fatal(err)
	}
	rendered := RenderInjectable(got)
	if !strings.Contains(rendered, "DATA, never policy") {
		t.Fatalf("rendered block has no header: %q", rendered)
	}
	if RenderInjectable(nil) != "" {
		t.Fatal("an empty block rendered a header with no content")
	}
}

func TestBudgetIsRespected(t *testing.T) {
	s := open(t)
	for i, id := range []string{"e1", "e2", "e3"} {
		if err := s.AppendWithTicket("t"+id, Entry{
			ID: id, Kind: "note", Body: strings.Repeat("x", 100), Producer: "broker",
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	got, err := s.Injectable(250)
	if err != nil {
		t.Fatal(err)
	}
	total := len(InjectableHeader)
	for _, e := range got {
		total += len(e.Body)
	}
	if total > 250 {
		t.Fatalf("injected %d chars (header included) against a 250 budget", total)
	}
	// It must be the most that fits, not merely something that fits: the header counts
	// against the budget, so 250 yields one 100-char entry, not two.
	oneMore := total + 100
	if oneMore <= 250 {
		t.Fatalf("stopped at %d entries with room for another (%d <= 250)", len(got), oneMore)
	}
}

// Eviction is not deletion: the body survives, because an evicted entry is still
// evidence about what the agent was told.
func TestEvictionKeepsTheBodyAndDropsItFromReads(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "something the agent was told")
	n, err := s.Evict([]string{"e1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if _, err := s.Get("e1"); err == nil {
		t.Fatal("an evicted entry is still readable")
	}
	// The row and its body are still in the table, which is the point.
	var body string
	var evictedAt *int64
	if err := s.DB().QueryRow(`SELECT body, evicted_at FROM gov_entry WHERE id='e1'`).Scan(&body, &evictedAt); err != nil {
		t.Fatal(err)
	}
	if body == "" || evictedAt == nil {
		t.Fatalf("body=%q evicted_at=%v — the row was altered rather than marked", body, evictedAt)
	}
}

// One-way: a budget squeeze cannot be walked back by a later writer.
func TestEvictionCannotBeReversed(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "x")
	if _, err := s.Evict([]string{"e1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE gov_entry SET evicted_at = NULL WHERE id='e1'`); err == nil {
		t.Fatal("eviction was reversed")
	}
}

// Eviction must not become a way to rewrite the body.
func TestEvictionCannotTouchTheBody(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "original")
	if _, err := s.DB().Exec(`UPDATE gov_entry SET evicted_at = 1, body = 'replaced' WHERE id='e1'`); err == nil {
		t.Fatal("eviction carried a body rewrite")
	}
}

func TestEvictToBudgetKeepsPinnedAndNewestFirst(t *testing.T) {
	s := open(t)
	for _, id := range []string{"old", "mid", "new"} {
		seed(t, s, id, strings.Repeat("-", 100))
		time.Sleep(2 * time.Millisecond)
	}
	n, err := s.EvictToBudget(250, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("evicted %d, want 1 to reach 250 from 300", n)
	}
	if _, err := s.Get("old"); err != nil {
		t.Fatal("a pinned entry was evicted")
	}
	if _, err := s.Get("new"); err != nil {
		t.Fatal("the newest entry was evicted before the oldest")
	}
}

// The block is spent in the order it is rendered, so the best-justified content has to be
// spent first: a fact ahead of an internal observation. The class comes from the closure,
// not from the row, so this is also a check that promotion raises the effective class.
func TestFactsSortAboveInternalEntries(t *testing.T) {
	s := open(t)
	// Two independent ticketed roots: one origin would be one root, and one root cannot
	// promote, which is the launder the root closure exists to catch.
	mustTicketAppend(t, s, "tkt-r1", "r1", "the cluster has three nodes")
	mustTicketAppend(t, s, "tkt-r2", "r2", "the cluster has four nodes")
	if err := s.Append(Entry{
		ID: "derived", Kind: "summary", Body: "the cluster has three or four nodes",
		SourceKind: TaintInternal, Parents: []string{"r1", "r2"},
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("derived append: %v", err)
	}
	if err := s.Promote("derived", "operator"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	mustTicketAppend(t, s, "tkt-i1", "i1", "an internal note")

	got, err := s.Injectable(4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("injectable = %d entries, want 4: %+v", len(got), got)
	}
	// Nothing weaker may precede something stronger.
	for i := 1; i < len(got); i++ {
		if taintRank[got[i-1].Effective] < taintRank[got[i].Effective] {
			t.Fatalf("entry %d (%s) outranks entry %d (%s): %+v",
				i-1, got[i-1].Effective, i, got[i].Effective, got)
		}
	}
	if got[0].ID != "derived" || got[0].Effective != TaintFact {
		t.Fatalf("first entry = %s/%s, want the promoted fact", got[0].ID, got[0].Effective)
	}
	// Within a class the newest-first order of the candidates survives: the sort is
	// stable, so it groups by provenance without reordering what it grouped.
	if got[1].ID != "i1" {
		t.Fatalf("internal entries reordered: %s first, want the newest (i1)", got[1].ID)
	}
}

func mustTicketAppend(t *testing.T, s *Store, ticket, id, body string) {
	t.Helper()
	if err := s.AppendWithTicket(ticket, Entry{
		ID: id, Kind: "note", Body: body, Producer: "broker",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("ticketed append %s: %v", id, err)
	}
}
