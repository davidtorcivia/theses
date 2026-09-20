package core

import (
	"context"
	"database/sql"
	"time"
)

// CompactAfter is how old every row of a run must be before it is folded. It
// has to stay above the lifetime of a client key: a command of several steps
// spends one key per step against a separate activity row, and folding one of
// those away while its key is live would let a replay miss the first step, hit
// the rest, and write the same paragraph twice.
const CompactAfter = 48 * time.Hour

// compactGap ends a run. A pause longer than this is somebody coming back to
// the paragraph, not still typing it.
const compactGap = 10 * time.Minute

// A run is folded whole or not at all, so the work is divided by block rather
// than by row: compactRows is where the next block is left for the next
// transaction, and one block holding more rows than that is still done in one
// go. compactBlocks bounds the other way, for a log of many blocks with few
// rows each, where counting their rows is itself the work.
//
// A generated log of 200,000 saves over 200 blocks folds in ten chunks, five
// seconds altogether and at most six tenths of a second in any one
// transaction. Unchunked it was one transaction of five seconds, which is
// longer than the five second busy timeout a writer waits on the lock. Six
// tenths of a second is what dividing that log by block came to and not a
// bound: one block holding all 200,000 of those saves is one transaction of
// about two and a half seconds, because a run cannot be folded in halves.
const (
	compactRows   = 20000
	compactBlocks = 500
)

// Compact folds runs of typed saves into one activity row each and returns how
// many rows it removed. A document saves itself as it is typed, so a person
// writing one paragraph leaves a row about once a second, each holding the
// whole block before and after; a day later nobody wants to read that second by
// second and nothing else needs it either, because the merge bases live in
// block_texts.
//
// A run is a maximal sequence of block.set rows for one block by one actor
// (kind, id and via), with no other row for that block between them and no
// pause longer than compactGap. A run every one of whose rows is older than
// olderThan, and which is longer than one row, keeps its last row with the
// first row's before_json and loses the rest. The kept row therefore says who
// changed the block from the text before they started to the text when they
// stopped, which is what the log promises, and it keeps its id, so a client
// catching up through Since still converges on it. A row that has been undone
// is left alone and ends the run either side of it, because folding it away
// would say a change was undone that was not.
//
// This publishes nothing and records nothing: it changes how the log says what
// happened, not what happened.
func (s *Service) Compact(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := s.Now().Add(-olderThan).Unix()
	total := 0
	for from := ""; ; {
		var to sql.NullString
		if err := s.DB.QueryRowContext(ctx, chunkQuery,
			from, compactBlocks, compactRows).Scan(&to); err != nil {
			return total, err
		}
		if !to.Valid {
			return total, nil
		}
		removed, err := s.fold(ctx, from, to.String, cutoff)
		if err != nil {
			return total, err
		}
		total += removed
		from = to.String
	}
}

// fold folds the runs of the blocks after from and up to and including to, in
// one transaction.
func (s *Service) fold(ctx context.Context, from, to string, cutoff int64) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// The runs are found once and written down, because the update and the
	// delete need the same answer and the pass over the log is the expensive
	// half. Temp DDL is transactional, so a failure anywhere below takes the
	// table with it.
	if _, err := tx.ExecContext(ctx, foldQuery,
		from, to, int64(compactGap/time.Second), cutoff); err != nil {
		return 0, err
	}
	// Without this the update below reads the whole of fold once per run.
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX fold_id ON fold(id)`); err != nil {
		return 0, err
	}
	// The kept row is given the run's opening before first: a delete without it
	// would throw away the text the run started from.
	if _, err := tx.ExecContext(ctx, `UPDATE activity SET before_json =
		(SELECT opened.before_json FROM activity opened
			WHERE opened.id = (SELECT first_id FROM fold WHERE fold.id = activity.id))
		WHERE id IN (SELECT last_id FROM fold)`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM activity WHERE id IN (SELECT id FROM fold WHERE id <> last_id)`)
	if err != nil {
		return 0, err
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// This drop is the one that matters: the rollback above cannot reach a
	// table the commit has kept, and the connection goes back to the pool with
	// whatever temp tables it still holds, so the next fold on it would find
	// this one in its way.
	if _, err := tx.ExecContext(ctx, `DROP TABLE temp.fold`); err != nil {
		return 0, err
	}
	return int(removed), tx.Commit()
}

// chunkQuery names the last block of the next chunk: the blocks after the
// cursor, at most as many as the second parameter, and no more of them than the
// third parameter counts rows for, except that a single block over that count
// is a chunk of its own. It is NULL when the cursor has passed the last block.
const chunkQuery = `WITH blocks AS (
	SELECT entity_id, count(*) AS held FROM activity
	WHERE entity = 'block' AND entity_id > ?
	GROUP BY entity_id ORDER BY entity_id LIMIT ?
)
SELECT coalesce(
	(SELECT max(entity_id) FROM
		(SELECT entity_id, sum(held) OVER (ORDER BY entity_id) AS running FROM blocks)
		WHERE running <= ?),
	(SELECT min(entity_id) FROM blocks))`

// foldQuery writes every row of every foldable run into a temp table, with the
// ids of the rows its run opened and closed with. The parameters are the block
// range, the gap in seconds that ends a run, and the moment a run has to lie
// wholly before.
const foldQuery = `CREATE TEMP TABLE fold AS
WITH log AS (
	SELECT id, entity_id, created_at,
		CASE WHEN action = 'set' AND undone_at IS NULL THEN 1 ELSE 0 END AS typed,
		actor_kind || char(31) || actor_id || char(31) || coalesce(via, '') AS who
	FROM activity WHERE entity = 'block' AND entity_id > ? AND entity_id <= ?
), edges AS (
	SELECT id, entity_id, created_at, typed,
		CASE WHEN typed = 1
			AND lag(typed) OVER block = 1
			AND lag(who) OVER block = who
			AND created_at - lag(created_at) OVER block <= ?
		THEN 0 ELSE 1 END AS opens
	FROM log WINDOW block AS (PARTITION BY entity_id ORDER BY id)
), numbered AS (
	SELECT id, entity_id, created_at, typed,
		sum(opens) OVER (PARTITION BY entity_id ORDER BY id) AS span
	FROM edges
), spans AS (
	SELECT id, typed,
		min(id) OVER whole AS first_id,
		max(id) OVER whole AS last_id,
		count(*) OVER whole AS held,
		max(created_at) OVER whole AS newest
	FROM numbered WINDOW whole AS (PARTITION BY entity_id, span)
)
SELECT id, first_id, last_id FROM spans WHERE typed = 1 AND held > 1 AND newest < ?`
