// Package memstore is the governed shared-memory substrate: one append-only
// SQLite database whose guarantees are enforced by SQL rather than by the code
// that happens to use it.
//
// The premise is narrow and worth stating, because it is what the whole package is
// for: the store is only meaningful if a write that should not happen *cannot*
// happen. An append-only table guarded by a Go wrapper is guarded by whoever calls
// the wrapper, and the caller is the thing under governance. So every invariant
// here — append-only, no delete, expiry, searchability — is a trigger, a CHECK, or
// a view. Nothing depends on the writer's cooperation.
package memstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the store builds in the same places gaap does
)

// Store is a handle on the governed memory database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// ErrCycle is returned when an edge would create a cycle.
var ErrCycle = errors.New("memstore: edge would create a cycle")

// ErrUnreachable is returned when a row exists but is not reachable to a reader:
// tombstoned, expired, superseded or evicted. The distinction from "no such row"
// is deliberately not exposed — a caller who can tell them apart can probe for
// tombstones.
var ErrUnreachable = errors.New("memstore: entry not reachable")

// MaxDepth bounds a lineage walk. A memory store that follows edges without a
// limit is a store where one crafted write makes every later read expensive.
const MaxDepth = 32

// Open opens or creates the store at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One writer. SQLite's own locking is the authority; a pool that lets two
	// goroutines race a write produces lock errors that look like policy failures.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memstore: apply schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests that must attempt SQL the API refuses.
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS gov_entry (
	id           TEXT PRIMARY KEY,
	root_id      TEXT NOT NULL,
	kind         TEXT NOT NULL,
	body         TEXT NOT NULL,
	source_kind  TEXT NOT NULL,
	source_ref   TEXT,
	-- taint_own is set by the governor from this row's own source at write time and
	-- is immutable by trigger. It is NOT taint_eff: the effective class is computed
	-- at read from the derived_from closure, because a stored value is a value a
	-- writer can be argued into setting.
	taint_own    TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','tombstoned')),
	created_at   INTEGER NOT NULL,
	-- > 0, unconditionally. v2 of the design required a NoExpiry grant to write
	-- this column, and the column's own CHECK forbade the value that grant implied.
	expires_at   INTEGER NOT NULL CHECK (expires_at > 0),
	superseded_by TEXT,
	evicted_at   INTEGER
);

CREATE INDEX IF NOT EXISTS gov_entry_root ON gov_entry(root_id);

CREATE TABLE IF NOT EXISTS gov_edge (
	child_id  TEXT NOT NULL REFERENCES gov_entry(id),
	parent_id TEXT NOT NULL REFERENCES gov_entry(id),
	PRIMARY KEY (child_id, parent_id)
);

CREATE INDEX IF NOT EXISTS gov_edge_parent ON gov_edge(parent_id);

CREATE TABLE IF NOT EXISTS gov_resolution (
	entry_id    TEXT NOT NULL REFERENCES gov_entry(id),
	resolution  TEXT NOT NULL CHECK (resolution IN ('confirmed','refuted','superseded')),
	resolved_by TEXT NOT NULL,
	resolved_at INTEGER NOT NULL,
	reason      TEXT
);

-- Pending is DERIVED: an entry with no resolution row is pending. Storing the
-- pending state would make it a value the writer sets, and a resolver that forgets
-- to clear a flag silently marks a confirmed fact pending forever.
CREATE VIEW IF NOT EXISTS gov_current AS
SELECT e.id,
       e.root_id,
       e.kind,
       e.body,
       e.source_kind,
       e.taint_own,
       e.expires_at,
       COALESCE(r.resolution, 'pending') AS resolution,
       r.resolved_by,
       r.resolved_at
FROM gov_entry e
LEFT JOIN (
	SELECT entry_id, resolution, resolved_by, resolved_at,
	       ROW_NUMBER() OVER (PARTITION BY entry_id ORDER BY resolved_at DESC) AS rn
	FROM gov_resolution
) r ON r.entry_id = e.id AND r.rn = 1
WHERE e.status = 'active'
  AND e.superseded_by IS NULL
  AND e.evicted_at IS NULL;

-- Append-only, enforced here rather than in Go. The single legal transition is the
-- one-way tombstone: active -> tombstoned with the body cleared. Everything else —
-- rewriting the body, the taint, the expiry, the source — aborts.
CREATE TRIGGER IF NOT EXISTS gov_entry_append_only
BEFORE UPDATE ON gov_entry
FOR EACH ROW
WHEN NOT (
	OLD.status = 'active'
	AND NEW.status = 'tombstoned'
	AND NEW.body = ''
	AND NEW.id = OLD.id
	AND NEW.root_id = OLD.root_id
	AND NEW.kind = OLD.kind
	AND NEW.source_kind = OLD.source_kind
	AND COALESCE(NEW.source_ref,'') = COALESCE(OLD.source_ref,'')
	AND NEW.taint_own = OLD.taint_own
	AND NEW.created_at = OLD.created_at
	AND NEW.expires_at = OLD.expires_at
	AND COALESCE(NEW.superseded_by,'') = COALESCE(OLD.superseded_by,'')
	AND COALESCE(NEW.evicted_at,0) = COALESCE(OLD.evicted_at,0)
)
BEGIN
	SELECT RAISE(ABORT, 'gov_entry is append-only: only a one-way tombstone is permitted');
END;

CREATE TRIGGER IF NOT EXISTS gov_entry_no_delete
BEFORE DELETE ON gov_entry
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, 'gov_entry refuses DELETE: the record is a tombstone, not a gap');
END;

CREATE TRIGGER IF NOT EXISTS gov_edge_no_delete
BEFORE DELETE ON gov_edge
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, 'gov_edge refuses DELETE: lineage is not retractable');
END;

CREATE TRIGGER IF NOT EXISTS gov_resolution_append_only
BEFORE UPDATE ON gov_resolution
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, 'gov_resolution is append-only');
END;

CREATE TRIGGER IF NOT EXISTS gov_resolution_no_delete
BEFORE DELETE ON gov_resolution
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, 'gov_resolution refuses DELETE');
END;

-- Search is an external-content FTS index. All three sync triggers are required:
-- with only the insert trigger, the one legal UPDATE (body -> '') leaves the old
-- body searchable, which is a tombstone you can still read.
CREATE VIRTUAL TABLE IF NOT EXISTS gov_entry_fts USING fts5(
	body,
	content='gov_entry',
	content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS gov_entry_fts_insert AFTER INSERT ON gov_entry
BEGIN
	INSERT INTO gov_entry_fts(rowid, body) VALUES (new.rowid, new.body);
END;

CREATE TRIGGER IF NOT EXISTS gov_entry_fts_update AFTER UPDATE ON gov_entry
BEGIN
	INSERT INTO gov_entry_fts(gov_entry_fts, rowid, body) VALUES ('delete', old.rowid, old.body);
	INSERT INTO gov_entry_fts(rowid, body) VALUES (new.rowid, new.body);
END;

CREATE TRIGGER IF NOT EXISTS gov_entry_fts_delete AFTER DELETE ON gov_entry
BEGIN
	INSERT INTO gov_entry_fts(gov_entry_fts, rowid, body) VALUES ('delete', old.rowid, old.body);
END;
`
