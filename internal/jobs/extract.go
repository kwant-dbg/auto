package jobs

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/extract"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
)

// ExtractOptions bound extraction scheduling.
type ExtractOptions struct {
	MaxQueuedTasks   int // backpressure: queued+running extract tasks
	MaxDocsPerBatch  int
	MaxBytesPerBatch int
	MaxAttempts      int // per document, across tasks
}

func (o *ExtractOptions) defaults() {
	if o.MaxQueuedTasks <= 0 {
		o.MaxQueuedTasks = 8
	}
	if o.MaxDocsPerBatch <= 0 {
		o.MaxDocsPerBatch = 20
	}
	if o.MaxBytesPerBatch <= 0 {
		o.MaxBytesPerBatch = 60_000
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
}

type candidate struct {
	id, sourceID int64
	size         int
}

// ScheduleExtraction batches documents that have no extraction yet (or whose
// extraction task died with attempts left) into extract tasks. Documents are
// marked as queued in the same transaction, so they're never batched twice.
// Batches hold documents from one source, bounded by count and bytes.
func ScheduleExtraction(ctx context.Context, st *store.Store, opt ExtractOptions) (enqueued int, err error) {
	opt.defaults()
	version := extract.PromptVersion
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7004135)`); err != nil {
			return err
		}
		// Rows whose task died: give up once attempts are spent.
		if _, err := tx.Exec(ctx, `
			UPDATE extraction e SET status = 'failed', updated_at = now()
			FROM task t
			WHERE e.task_id = t.id AND t.state = 'dead' AND e.status = 'queued' AND e.attempts >= $1`, opt.MaxAttempts); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM task WHERE type = $1 AND state IN ('queued', 'running')`,
			extract.TaskType).Scan(&active); err != nil {
			return err
		}
		slots := opt.MaxQueuedTasks - active
		if slots <= 0 {
			return nil
		}
		// Interleave sources (each source's 1st document, then each one's 2nd,
		// …) so one source's backlog can't starve the others.
		rows, err := tx.Query(ctx, `
			SELECT id, source_id, byte_size FROM (
				SELECT r.id, r.source_id, r.byte_size,
				       row_number() OVER (PARTITION BY r.source_id ORDER BY r.id) AS rn
				FROM raw_document r
				LEFT JOIN extraction e ON e.raw_document_id = r.id AND e.prompt_version = $1
				LEFT JOIN task t ON t.id = e.task_id
				WHERE e.raw_document_id IS NULL
				   OR (e.status = 'queued' AND (t.id IS NULL OR t.state = 'dead') AND e.attempts < $2)
			) x
			ORDER BY rn, source_id
			LIMIT $3`, version, opt.MaxAttempts, slots*opt.MaxDocsPerBatch)
		if err != nil {
			return err
		}
		cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
			var c candidate
			err := r.Scan(&c.id, &c.sourceID, &c.size)
			return c, err
		})
		if err != nil {
			return err
		}
		// The interleaved selection is regrouped by source for batching.
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].sourceID != cands[j].sourceID {
				return cands[i].sourceID < cands[j].sourceID
			}
			return cands[i].id < cands[j].id
		})

		for _, batch := range interleave(Batches(cands, opt.MaxDocsPerBatch, opt.MaxBytesPerBatch)) {
			if enqueued >= slots {
				break
			}
			ids := make([]int64, len(batch))
			for i, c := range batch {
				ids[i] = c.id
			}
			taskID, _, err := queue.Enqueue(ctx, tx, queue.NewTask{
				Type:   extract.TaskType,
				Inputs: extract.Inputs{DocumentIDs: ids, PromptVersion: version},
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO extraction (raw_document_id, prompt_version, status, task_id)
				SELECT unnest($1::bigint[]), $2, 'queued', $3
				ON CONFLICT (raw_document_id, prompt_version) DO UPDATE SET
					status = 'queued', task_id = EXCLUDED.task_id, attempts = extraction.attempts + 1, updated_at = now()`,
				ids, version, taskID); err != nil {
				return fmt.Errorf("mark queued: %w", err)
			}
			enqueued++
		}
		return nil
	})
	return enqueued, err
}

// interleave orders batches round-robin by source, so the slot limit cuts
// evenly across sources instead of dropping the last source's batches.
func interleave(batches [][]candidate) [][]candidate {
	var order []int64
	bySource := map[int64][][]candidate{}
	for _, b := range batches {
		src := b[0].sourceID
		if _, ok := bySource[src]; !ok {
			order = append(order, src)
		}
		bySource[src] = append(bySource[src], b)
	}
	out := make([][]candidate, 0, len(batches))
	for len(out) < len(batches) {
		for _, src := range order {
			if q := bySource[src]; len(q) > 0 {
				out = append(out, q[0])
				bySource[src] = q[1:]
			}
		}
	}
	return out
}

// Batches groups candidates (sorted by source) into batches from a single
// source, each within maxDocs and maxBytes. A document larger than maxBytes
// gets a batch of its own.
func Batches(cands []candidate, maxDocs, maxBytes int) [][]candidate {
	var out [][]candidate
	var cur []candidate
	size := 0
	for _, c := range cands {
		if len(cur) > 0 && (c.sourceID != cur[0].sourceID || len(cur) >= maxDocs || size+c.size > maxBytes) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, c)
		size += c.size
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
