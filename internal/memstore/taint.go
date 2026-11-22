package memstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Provenance classes, worst to best. "Effective" taint is the WORST class in a chain:
// a summary of an external observation is still external, however well it is written.
var taintRank = map[string]int{
	TaintExternal: 0,
	TaintInternal: 1,
	TaintFact:     2,
}

// ErrProducerCannotPromote is returned when a row's writer tries to certify its own
// content.
var ErrProducerCannotPromote = errors.New("memstore: the producer of an entry cannot promote it")

// ErrInsufficientRoots is returned when a promotion lacks two independent roots.
var ErrInsufficientRoots = errors.New("memstore: promotion needs at least two distinct roots")

// ErrTicketReused is returned when a ticket was already consumed.
var ErrTicketReused = errors.New("memstore: ticket already consumed")

// EffectiveTaint returns the provenance a reader may rely on for an entry.
//
// Computed on every read, never stored. A stored effective taint is a value that can
// disagree with the lineage it summarises, and the disagreement is invisible: the row
// says `fact` and the ancestry says otherwise, and only one of them is evidence.
//
// own is this row's immutably-recorded class; effective is the worst along the path
// that produced it.
func (s *Store) EffectiveTaint(id string) (own, effective string, err error) {
	rows, err := s.db.QueryContext(context.Background(), `WITH RECURSIVE anc(node, depth) AS (
			SELECT ?, 0
			UNION
			SELECT e.parent_id, anc.depth + 1
			FROM gov_edge e JOIN anc ON e.child_id = anc.node
			WHERE anc.depth < ?
		)
		SELECT e.id, e.taint_own FROM gov_entry e JOIN anc ON e.id = anc.node`,
		id, MaxDepth)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rows.Close() }()

	found := false
	worst := 99
	for rows.Next() {
		var nodeID, taint string
		if err := rows.Scan(&nodeID, &taint); err != nil {
			return "", "", err
		}
		if nodeID == id {
			own = taint
			found = true
		}
		if r, ok := taintRank[taint]; ok && r < worst {
			worst = r
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if !found {
		return "", "", ErrUnreachable
	}
	effective = TaintExternal
	for cls, r := range taintRank {
		if r == worst {
			effective = cls
		}
	}

	// A promotion can raise the effective class, and only above the lineage's floor:
	// two independent roots agreeing is evidence the single chain did not have.
	promoted, err := s.promotedRoots(id)
	if err != nil {
		return "", "", err
	}
	if promoted >= 2 && taintRank[effective] < taintRank[TaintFact] {
		effective = TaintFact
	}
	return own, effective, nil
}

// promotedRoots returns the distinct-root count recorded for a promotion, or 0.
//
// The count is re-validated against the live lineage rather than trusted from the
// promotion row: the row records what was true when it was written, and the lineage is
// append-only, so a live count can only be equal or larger. Trusting the column would
// mean a corrupted row could assert a promotion the graph does not support.
func (s *Store) promotedRoots(id string) (int, error) {
	var recorded int
	err := s.db.QueryRow(`SELECT distinct_roots FROM gov_promotion WHERE entry_id = ?`, id).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	live, err := s.distinctRoots(id)
	if err != nil {
		return 0, err
	}
	if live < recorded {
		return live, nil
	}
	return recorded, nil
}

// distinctRoots counts the distinct root ids in an entry's ancestry, including itself.
//
// Roots rather than entries: five summaries of one source are five entries and one
// origin, and counting entries would let a single source promote itself by being
// paraphrased. This is the number the design's "two independent roots" means.
func (s *Store) distinctRoots(id string) (int, error) {
	var n int
	// gov_root is already transitive — Append copies its parents' root rows — so this is
	// a straight count, not a walk. Walking gov_entry.root_id instead would count the
	// entry's own id as a root for any row appended without an explicit one.
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT root_id) FROM gov_root WHERE entry_id = ?`, id).Scan(&n)
	return n, err
}

// Promote records that a principal other than the producer vouches for an entry, on
// the strength of at least two distinct roots.
func (s *Store) Promote(entryID, promoter string) error {
	if promoter == "" {
		return errors.New("memstore: a promotion must name who promoted it")
	}
	var producer string
	err := s.db.QueryRow(`SELECT COALESCE(producer,'') FROM gov_entry WHERE id = ?`, entryID).Scan(&producer)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnreachable
	}
	if err != nil {
		return err
	}
	if producer != "" && producer == promoter {
		return fmt.Errorf("%w: %s", ErrProducerCannotPromote, promoter)
	}
	roots, err := s.distinctRoots(entryID)
	if err != nil {
		return err
	}
	if roots < 2 {
		return fmt.Errorf("%w: found %d", ErrInsufficientRoots, roots)
	}
	_, err = s.db.Exec(`INSERT INTO gov_promotion(entry_id, promoted_by, promoted_at, distinct_roots)
		VALUES (?,?,?,?)`, entryID, promoter, s.now().Unix(), roots)
	if err != nil {
		return fmt.Errorf("memstore: promote %s: %w", entryID, err)
	}
	return nil
}

// ConsumeTicket burns a gate ticket id. Single use is a primary key, not a flag: a flag
// can be reset, and the row that says "already used" cannot be.
//
// The caller passes its transaction so redemption and the write it authorises commit
// together — a ticket consumed in one transaction and used in another is a ticket that
// can be lost after the work it paid for.
func (s *Store) ConsumeTicket(tx *sql.Tx, ticketID string) error {
	if ticketID == "" {
		return errors.New("memstore: ticket id is required")
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM gov_ticket WHERE id = ?`, ticketID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrTicketReused
	}
	_, err := tx.Exec(`INSERT INTO gov_ticket(id, consumed_at) VALUES (?,?)`, ticketID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("memstore: consume ticket: %w", err)
	}
	return nil
}

// AppendWithTicket appends an entry claiming internal provenance, in exchange for a
// single-use ticket.
//
// This is the only path that can produce `internal` from a call result. Everything else
// is derived from what the row cites; this is derived from what the platform attested.
func (s *Store) AppendWithTicket(ticketID string, e Entry) error {
	if ticketID == "" {
		return errors.New("memstore: an internal append requires a ticket")
	}
	if len(e.Parents) > 0 {
		return errors.New("memstore: an append with a ticket must not also cite parents; the ticket is the provenance")
	}
	e.SourceKind = TaintInternal
	e.Producer = producerOf(e)
	if e.Producer == "" {
		return errors.New("memstore: an append with a ticket must name its producer")
	}
	e.RootID = orSelf(e.RootID, e.ID)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.ConsumeTicket(tx, ticketID); err != nil {
		return err
	}
	if err := insertEntry(tx, e, TaintInternal, e.Producer, s.now().Unix()); err != nil {
		return err
	}
	// A ticketed observation is a root: nothing derived it from another entry.
	if _, err := tx.Exec(`INSERT INTO gov_root(entry_id, root_id) VALUES (?,?)`, e.ID, e.ID); err != nil {
		return fmt.Errorf("memstore: root %s: %w", e.ID, err)
	}
	return tx.Commit()
}

func orSelf(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
