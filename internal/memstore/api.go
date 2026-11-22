package memstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aj-nt/gaap/platform"
)

// Taint classes come from the kernel, not from this package. The store records
// taint_own only; the effective class of a read is computed from the lineage.
//
// Aliased rather than redeclared: three modules spelling their own provenance strings
// is how "external" in the store stops meaning "external" in the governor, and the
// drift is silent because both are valid-looking strings.
const (
	TaintExternal = string(platform.TaintExternal)
	TaintInternal = string(platform.TaintInternal)
	TaintFact     = string(platform.TaintFact)
)

// Entry is one memory record.
type Entry struct {
	ID         string
	RootID     string
	Kind       string
	Body       string
	SourceKind string
	SourceRef  string
	// Producer names what wrote the row, so a promoter can be checked against it.
	Producer  string
	TaintOwn  string
	ExpiresAt time.Time
	// Parents are the entries this one was derived from. They are what makes
	// launderable content visible: a rephrasing that cites nothing is external by
	// definition, whatever it claims about itself.
	Parents []string
}

// Append writes an entry and its lineage edges in one transaction.
//
// Taint is derived here, not accepted from the caller: an API that takes the taint
// as an argument is an API where a test — or a bug — can inject `fact`. The rule is
// the one the design states: a row with no cited parent is external, and nothing in
// the body can raise that.
func (s *Store) Append(e Entry) error {
	if e.ID == "" {
		return errors.New("memstore: entry id is required")
	}
	if e.RootID == "" {
		e.RootID = e.ID
	}
	// The taint is derived, never accepted. A row with no cited parent is external;
	// a row with one is internal because its provenance is at least inspectable. A
	// caller asserting higher provenance than that is refused rather than downgraded:
	// silently recording external and returning success teaches the caller its
	// assertion worked.
	var taint string
	if len(e.Parents) == 0 {
		if e.SourceKind == TaintInternal || e.SourceKind == TaintFact {
			return fmt.Errorf("memstore: entry %s claims source_kind %q with no cited parent", e.ID, e.SourceKind)
		}
		taint = TaintExternal
	} else {
		// A parent must exist: lineage pointing at nothing is a chain a reader walks
		// into a void, and FK enforcement is the only place that is checkable.
		taint = TaintInternal
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertEntry(tx, e, taint, producerOf(e), s.now().Unix()); err != nil {
		return err
	}
	for _, p := range e.Parents {
		if err := addEdge(tx, e.ID, p); err != nil {
			return err
		}
	}
	// The origin closure is inherited from the parents, so a row never becomes its own
	// root merely by citing someone. A parent with no root row is a lineage hole and is
	// treated as its own origin rather than silently dropped.
	if len(e.Parents) == 0 {
		if _, err := tx.Exec(`INSERT INTO gov_root(entry_id, root_id) VALUES (?,?)`, e.ID, e.ID); err != nil {
			return fmt.Errorf("memstore: root %s: %w", e.ID, err)
		}
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(e.Parents)), ",")
		args := make([]any, 0, len(e.Parents)+1)
		args = append(args, e.ID)
		for _, p := range e.Parents {
			args = append(args, p)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO gov_root(entry_id, root_id)
			SELECT ?, root_id FROM gov_root WHERE entry_id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("memstore: inherit roots of %s: %w", e.ID, err)
		}
		// No fallback to the parent's own id here. Treating a lineage hole as an origin
		// is exactly the launder this table exists to stop: a parent with no roots
		// contributes none, and an entry that ends up with too few roots simply cannot be
		// promoted.
	}
	return tx.Commit()
}

// insertEntry writes the row itself. Both append paths share it, because the column list
// is the store's record of what happened and a column added to one path and not the other
// is half a database whose readers cannot predict its shape.
func insertEntry(tx *sql.Tx, e Entry, taint, producer string, now int64) error {
	_, err := tx.Exec(`INSERT INTO gov_entry
		(id, root_id, kind, body, source_kind, source_ref, taint_own, status, created_at, expires_at, producer)
		VALUES (?,?,?,?,?,?,?, 'active', ?, ?, ?)`,
		e.ID, e.RootID, e.Kind, e.Body, e.SourceKind, nullable(e.SourceRef), taint,
		now, e.ExpiresAt.Unix(), producer)
	if err != nil {
		return fmt.Errorf("memstore: append %s: %w", e.ID, err)
	}
	return nil
}

// producerOf names what wrote the row, defaulting to what it was written about. Promotion
// compares this against the promoter, so an empty producer is a row nobody is accountable
// for — the default is a fallback, not a licence to leave it blank.
func producerOf(e Entry) string {
	if e.Producer != "" {
		return e.Producer
	}
	return e.SourceRef
}

// addEdge inserts child -> parent after refusing any cycle.
//
// The depth bound is what makes the recursion terminate: a cycle walks forever, and
// a query that never returns is worse than a rejected write — it is a rejected
// write that has already cost the process its availability.
func addEdge(tx *sql.Tx, child, parent string) error {
	var n int
	// Can parent already reach child by following its own ancestors? If so, this
	// edge closes a loop.
	err := tx.QueryRow(`WITH RECURSIVE up(node, depth) AS (
			SELECT ?, 0
			UNION ALL
			SELECT e.parent_id, up.depth + 1
			FROM gov_edge e JOIN up ON e.child_id = up.node
			WHERE up.depth < ?
		) SELECT COUNT(*) FROM up WHERE node = ?`, parent, MaxDepth, child).Scan(&n)
	if err != nil {
		return fmt.Errorf("memstore: cycle check: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %s -> %s", ErrCycle, child, parent)
	}
	if _, err := tx.Exec(`INSERT INTO gov_edge(child_id, parent_id) VALUES (?,?)`, child, parent); err != nil {
		return fmt.Errorf("memstore: edge %s -> %s: %w", child, parent, err)
	}
	return nil
}

// AddEdge adds a lineage edge between existing entries, refusing cycles.
func (s *Store) AddEdge(child, parent string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := addEdge(tx, child, parent); err != nil {
		return err
	}
	return tx.Commit()
}

// Tombstone is the one legal mutation: the row stays, the body stops being readable.
func (s *Store) Tombstone(id string) error {
	res, err := s.db.Exec(
		`UPDATE gov_entry SET status = 'tombstoned', body = '' WHERE id = ? AND status = 'active'`, id)
	if err != nil {
		return fmt.Errorf("memstore: tombstone %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUnreachable
	}
	return nil
}

// Resolve records a resolution. Pending is the absence of a row in this table, so
// there is no flag to clear and no way to leave one set.
func (s *Store) Resolve(entryID, resolution, decidedBy, reason string) error {
	if decidedBy == "" {
		return errors.New("memstore: a resolution must name who decided it")
	}
	_, err := s.db.Exec(`INSERT INTO gov_resolution(entry_id, resolution, resolved_by, resolved_at, reason)
		VALUES (?,?,?,?,?)`, entryID, resolution, decidedBy, s.now().Unix(), nullable(reason))
	if err != nil {
		return fmt.Errorf("memstore: resolve %s: %w", entryID, err)
	}
	return nil
}

// reachable is the predicate set every read shares. It is written once because a
// read path that filters expiry but not eviction is a read path that returns
// deleted content, and there is no way to see that from the outside.
const reachable = `
	e.status = 'active'
	AND e.superseded_by IS NULL
	AND e.evicted_at IS NULL
	AND e.expires_at > ?`

// Get returns one entry if it is reachable.
func (s *Store) Get(id string) (Entry, error) {
	row := s.db.QueryRow(`SELECT e.id, e.root_id, e.kind, e.body, e.source_kind,
			COALESCE(e.source_ref,''), e.taint_own, e.expires_at
		FROM gov_entry e WHERE e.id = ? AND `+reachable, id, s.now().Unix())
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrUnreachable
	}
	return e, err
}

// Search runs an FTS query and returns only reachable rows.
func (s *Store) Search(q string) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT e.id, e.root_id, e.kind, e.body, e.source_kind,
			COALESCE(e.source_ref,''), e.taint_own, e.expires_at
		FROM gov_entry_fts f
		JOIN gov_entry e ON e.rowid = f.rowid
		WHERE gov_entry_fts MATCH ? AND `+reachable+`
		ORDER BY e.created_at DESC`, q, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("memstore: search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolvePending reports whether an entry has an outstanding resolution. Derived,
// so it cannot disagree with gov_resolution.
func (s *Store) ResolvePending(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM gov_resolution WHERE entry_id = ?`, id).Scan(&n)
	return n == 0, err
}

// Current reads the gov_current view, which is the store's own statement of what is
// live. Used by tests to assert the view agrees with the read path rather than
// trusting either alone.
func (s *Store) Current() ([]Entry, error) {
	rows, err := s.db.Query(`SELECT id, root_id, kind, body, source_kind, '', taint_own, expires_at FROM gov_current`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type scannable interface{ Scan(...any) error }

func scanEntry(sc scannable) (Entry, error) {
	var e Entry
	var exp int64
	if err := sc.Scan(&e.ID, &e.RootID, &e.Kind, &e.Body, &e.SourceKind, &e.SourceRef, &e.TaintOwn, &exp); err != nil {
		return Entry{}, err
	}
	e.ExpiresAt = time.Unix(exp, 0)
	return e, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
