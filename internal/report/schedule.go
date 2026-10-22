package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
)

// DigestTaskType is the queue task type for the weekly digest.
const DigestTaskType = "digest"

// DigestInputs are the digest task's inputs.
type DigestInputs struct {
	Week string    `json:"week"`
	Due  time.Time `json:"due"`
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// DueAt is when the digest for t's ISO week is due (UTC).
func DueAt(opt Options, t time.Time) (time.Time, error) {
	opt.Defaults()
	wd, ok := weekdays[strings.ToLower(opt.Weekday)]
	if !ok {
		return time.Time{}, fmt.Errorf("digest weekday %q", opt.Weekday)
	}
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	offset := (int(wd) + 6) % 7 // days after Monday
	return monday.AddDate(0, 0, offset).Add(time.Duration(opt.Hour) * time.Hour), nil
}

// ScheduleDigest enqueues the week's digest once it is due.
func ScheduleDigest(ctx context.Context, st *store.Store, opt Options, now time.Time) (bool, error) {
	due, err := DueAt(opt, now)
	if err != nil || now.Before(due) {
		return false, err
	}
	week := ISOWeek(now)
	var done bool
	if err := st.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM digest WHERE week = $1)`, week).Scan(&done); err != nil || done {
		return false, err
	}
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
		Type: DigestTaskType, DedupeKey: "digest:" + week, Priority: 2, Inputs: DigestInputs{Week: week, Due: due},
	})
	return created, err
}
