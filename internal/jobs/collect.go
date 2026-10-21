// Package jobs wires task types to their handlers and defines the scheduled
// jobs that enqueue them.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kwant-dbg/auto/internal/collect"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/worker"
)

// TaskCollect collects one source.
const TaskCollect = "collect"

// CollectInputs are the inputs of a collect task.
type CollectInputs struct {
	Source string `json:"source"`
}

// CollectHandler runs a source's connector.
func CollectHandler(st *store.Store, opt collect.Options) worker.Handler {
	return func(ctx context.Context, t *queue.Task) (any, error) {
		var in CollectInputs
		if err := json.Unmarshal(t.Inputs, &in); err != nil {
			return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
		}
		src, err := st.GetSource(ctx, in.Source)
		if err != nil {
			return nil, queue.Permanent(err)
		}
		if !src.Enabled {
			return nil, queue.Permanent(fmt.Errorf("source %q is disabled", src.Name))
		}
		stats, err := collect.Run(ctx, st, src, opt)
		if errors.Is(err, collect.ErrAlreadyRunning) {
			return stats, queue.Permanent(err)
		}
		return stats, err
	}
}

// ScheduleCollections enqueues one collect task per due source. The dedupe
// key includes the source's scheduled run time, so each window is enqueued
// once however often the scheduler ticks. Collect tasks don't retry in the
// queue: a failed run reschedules itself sooner via the source's back-off.
func ScheduleCollections(ctx context.Context, st *store.Store, now time.Time) (enqueued int, err error) {
	due, err := st.DueSources(ctx, now)
	if err != nil {
		return 0, err
	}
	for _, src := range due {
		key := fmt.Sprintf("collect:%s:%d", src.Name, src.NextRunAt.Unix())
		id, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
			Type:        TaskCollect,
			DedupeKey:   key,
			MaxAttempts: 1,
			Inputs:      CollectInputs{Source: src.Name},
		})
		if err != nil {
			return enqueued, err
		}
		if created {
			enqueued++
			continue
		}
		// The window's task already exists. If it died without recording a
		// run (e.g. the worker crashed), reschedule so the source isn't stuck.
		var state string
		if err := st.DB.QueryRow(ctx, `SELECT state FROM task WHERE id = $1`, id).Scan(&state); err != nil {
			return enqueued, err
		}
		if state == "dead" {
			if err := st.RecordRun(ctx, src.ID, store.RunOutcome{Err: errors.New("previous collect task died without finishing")}); err != nil {
				return enqueued, err
			}
		}
	}
	return enqueued, nil
}
