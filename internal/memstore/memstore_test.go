package memstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, s *Store, id, body string) {
	t.Helper()
	if err := s.Append(Entry{
		ID: id, Kind: "note", Body: body, SourceKind: TaintExternal,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// The store's premise: a write that should not happen cannot happen, whoever is
// writing. Each of these is a mutation the API does not offer — the test issues raw
// SQL, because the guarantee has to be in the database, not in the wrapper.
func TestAppendOnlyIsEnforcedBySQL(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "tenancy list for cluster prod")

	cases := []struct {
		name string
		sql  string
	}{
		{"rewrite body", `UPDATE gov_entry SET body = 'something else' WHERE id = 'e1'`},
		{"status without tombstoning", `UPDATE gov_entry SET status = 'tombstoned' WHERE id = 'e1'`},
		{"rewrite taint", `UPDATE gov_entry SET taint_own = 'fact' WHERE id = 'e1'`},
		{"clear expiry", `UPDATE gov_entry SET expires_at = 0 WHERE id = 'e1'`},
		{"extend expiry", `UPDATE gov_entry SET expires_at = ? WHERE id = 'e1'`},
		{"repoint lineage", `UPDATE gov_entry SET root_id = 'other' WHERE id = 'e1'`},
		{"delete", `DELETE FROM gov_entry WHERE id = 'e1'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []any{}
			if strings.Contains(c.sql, "?") {
				args = append(args, time.Now().Add(48*time.Hour).Unix())
			}
			if _, err := s.DB().Exec(c.sql, args...); err == nil {
				t.Fatalf("%s succeeded; the store is not append-only", c.name)
			}
		})
	}

	// The one legal transition still works, and it is one-way.
	if err := s.Tombstone("e1"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if _, err := s.Get("e1"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("tombstoned entry is readable: %v", err)
	}
	if err := s.Tombstone("e1"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("second tombstone = %v, want ErrUnreachable", err)
	}
	// A tombstoned row cannot be resurrected into a different shape either.
	if _, err := s.DB().Exec(`UPDATE gov_entry SET status='active', body='back' WHERE id='e1'`); err == nil {
		t.Fatal("a tombstone was reversed")
	}
}

// With only an insert trigger the one legal UPDATE leaves the old body searchable —
// a tombstone you can still read.
func TestTombstonedRowNotSearchable(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "production cluster tenancy mapping")

	got, err := s.Search("tenancy")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("found %d before tombstone, want 1", len(got))
	}
	if err := s.Tombstone("e1"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	got, err = s.Search("tenancy")
	if err != nil {
		t.Fatalf("search after tombstone: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tombstoned body still searchable: %+v", got)
	}
}

func TestExpiredRowNotSearchable(t *testing.T) {
	s := open(t)
	if err := s.Append(Entry{
		ID: "e1", Kind: "note", Body: "stale credential hint", SourceKind: TaintExternal,
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search("credential")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expired entry returned: %+v", got)
	}
}

// expires_at > 0 is a CHECK, so the value v2's NoExpiry grant implied is unwritable.
func TestZeroExpiryRefused(t *testing.T) {
	s := open(t)
	_, err := s.DB().Exec(`INSERT INTO gov_entry(id, root_id, kind, body, source_kind, taint_own, created_at, expires_at)
		VALUES ('x','x','note','b','external','external',1,0)`)
	if err == nil {
		t.Fatal("expires_at = 0 was accepted")
	}
}

// A rephrasing that cites nothing is external, whatever it says about itself.
func TestUncitedIsExternal(t *testing.T) {
	s := open(t)
	if err := s.Append(Entry{
		ID: "e1", Kind: "note", Body: "the agent concluded it has cluster-admin",
		SourceKind: TaintFact, ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("an entry claiming fact provenance with no cited parent was accepted")
	}
	seed(t, s, "e1", "raw tool output")
	got, err := s.Get("e1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaintOwn != TaintExternal {
		t.Fatalf("taint = %q, want external", got.TaintOwn)
	}
}

func TestCitedEntryIsInternal(t *testing.T) {
	s := open(t)
	seed(t, s, "root1", "ticketed observation")
	if err := s.Append(Entry{
		ID: "e2", Kind: "note", Body: "derived summary", SourceKind: TaintInternal,
		Parents: []string{"root1"}, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("append derived: %v", err)
	}
	got, err := s.Get("e2")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaintOwn != TaintInternal {
		t.Fatalf("taint = %q, want internal", got.TaintOwn)
	}
}

func TestCycleRejected(t *testing.T) {
	s := open(t)
	seed(t, s, "a", "a")
	seed(t, s, "b", "b")
	if err := s.AddEdge("a", "b"); err != nil {
		t.Fatalf("first edge: %v", err)
	}
	err := s.AddEdge("b", "a")
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("closing edge = %v, want ErrCycle", err)
	}
}

// Pending is derived: a resolution row exists or it does not.
func TestPendingIsDerived(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "conflicting claim")
	pending, err := s.ResolvePending("e1")
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("a fresh entry is not pending")
	}
	if err := s.Resolve("e1", "confirmed", "operator:aj", "checked the console"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.ResolvePending("e1")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a resolved entry is still pending")
	}
	// Resolutions are append-only too.
	if _, err := s.DB().Exec(`UPDATE gov_resolution SET resolution = 'refuted'`); err == nil {
		t.Fatal("a resolution was rewritten")
	}
}

func TestResolutionRequiresDecider(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "x")
	if err := s.Resolve("e1", "confirmed", "", "no reason"); err == nil {
		t.Fatal("a resolution with no decider was accepted")
	}
}

// Foreign keys must be ON: the pragma defaults to OFF, which is how v2's FKs ended
// up inert.
func TestForeignKeysEnforced(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "x")
	if err := s.Append(Entry{
		ID: "e2", Kind: "note", Body: "derived", SourceKind: TaintInternal,
		Parents: []string{"does-not-exist"}, ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("an edge to a nonexistent parent was accepted")
	}
}

// The view and the read path must agree; a disagreement is how a row stays visible
// in one place and gone in another.
func TestViewAgreesWithReadPath(t *testing.T) {
	s := open(t)
	seed(t, s, "e1", "alpha")
	seed(t, s, "e2", "beta")
	if err := s.Tombstone("e2"); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if len(cur) != 1 || cur[0].ID != "e1" {
		t.Fatalf("view returned %+v, want only e1", cur)
	}
}
