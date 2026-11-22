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
