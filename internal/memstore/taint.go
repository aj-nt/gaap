package memstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// provenance is one entry's immutably-recorded class and the class a reader may rely on.
type provenance struct {
	own       string
	effective string
}

// EffectiveTaint returns the provenance a reader may rely on for an entry.
//
// Computed on every read, never stored. A stored effective taint is a value that can
// disagree with the lineage it summarises, and the disagreement is invisible: the row
// says `fact` and the ancestry says otherwise, and only one of them is evidence.
//
// own is this row's immutably-recorded class; effective is the worst along the path
// that produced it.
func (s *Store) EffectiveTaint(id string) (own, effective string, err error) {
	resolved, err := s.provenanceOf([]string{id})
	if err != nil {
		return "", "", err
	}
	p, ok := resolved[id]
	if !ok {
		return "", "", ErrUnreachable
	}
	return p.own, p.effective, nil
}

// provenanceOf resolves a whole set in three queries, whatever the set's size.
//
// The block that goes into every prompt needs the class of every candidate, and walking
// the lineage once per row made the cost of building a prompt grow with the size of
// memory rather than with what was in it. One closure for the set, one promotion lookup,
// one live root count.
//
// The rule lives here and only here: effective is the worst class in the chain, raised to
// fact where two independent roots are vouched for. Batched and single-entry resolution
// share this code so they cannot drift — a second copy of "what does the lineage say" is
// a second answer to the question that decides what reaches the prompt.
func (s *Store) provenanceOf(ids []string) (map[string]provenance, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string]provenance, len(ids))
	worst := make(map[string]int, len(ids))

	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, MaxDepth)
	rows, err := s.db.QueryContext(context.Background(), `WITH RECURSIVE anc(node, depth, start) AS (
			SELECT e.id, 0, e.id FROM gov_entry e WHERE e.id IN (`+placeholders(len(ids))+`)
			UNION
			SELECT p.parent_id, anc.depth + 1, anc.start
			FROM gov_edge p JOIN anc ON p.child_id = anc.node
			WHERE anc.depth < ?
		)
		SELECT anc.start, anc.node, e.taint_own
		FROM anc JOIN gov_entry e ON e.id = anc.node`,
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var start, node, taint string
		if err := rows.Scan(&start, &node, &taint); err != nil {
			return nil, err
		}
		p := out[start]
		if node == start {
			p.own = taint
		}
		out[start] = p
		if r, ok := taintRank[taint]; ok {
			if cur, seen := worst[start]; !seen || r < cur {
				worst[start] = r
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, r := range worst {
		p := out[id]
		p.effective = classOfRank(r)
		out[id] = p
	}
	if err := s.applyPromotions(out); err != nil {
		return nil, err
	}
	return out, nil
}

// applyPromotions raises a resolved class to fact where a voucher and two independent roots
// agree.
//
// The recorded count is re-validated against the live lineage rather than trusted from the
// promotion row: the row records what was true when it was written, and the lineage is
// append-only, so a live count can only be equal or larger. Trusting the column would mean
// a corrupted row could assert a promotion the graph does not support.
func (s *Store) applyPromotions(resolved map[string]provenance) error {
	if len(resolved) == 0 {
		return nil
	}
	ids := make([]any, 0, len(resolved))
	for id := range resolved {
		ids = append(ids, id)
	}
	in := placeholders(len(ids))

	recorded, err := s.idCounts(`SELECT entry_id, distinct_roots FROM gov_promotion WHERE entry_id IN (`+in+`)`, ids...)
	if err != nil {
		return err
	}
	if len(recorded) == 0 {
		return nil
	}

	liveIDs := make([]any, 0, len(recorded))
	for id := range recorded {
		liveIDs = append(liveIDs, id)
	}
	live, err := s.idCounts(`SELECT entry_id, COUNT(DISTINCT root_id) FROM gov_root
		WHERE entry_id IN (`+placeholders(len(liveIDs))+`) GROUP BY entry_id`, liveIDs...)
	if err != nil {
		return err
	}

	for id, rec := range recorded {
		promoted := min(rec, live[id])
		if promoted < 2 {
			continue
		}
		if p := resolved[id]; taintRank[p.effective] < taintRank[TaintFact] {
			p.effective = TaintFact
			resolved[id] = p
		}
	}
	return nil
}

// classOfRank is the inverse of taintRank. Ranks are unique, so the scan is deterministic.
func classOfRank(rank int) string {
	for cls, r := range taintRank {
		if r == rank {
			return cls
		}
	}
	return TaintExternal
}

// placeholders builds "?,?,?" for an IN list. The ids are still bound as arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
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

// idCounts runs a query returning (entry_id, count) and collects the answer. The two counts in
// applyPromotions differ in SQL, not in how a row is read — and a scan loop written twice is a
// loop where one copy forgets rows.Err().
func (s *Store) idCounts(query string, args ...any) (map[string]int, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
