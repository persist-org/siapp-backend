package patrol

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// The order rule: a guard walks a zone's checkpoints in order, round after round
// (docs/plan/10.1-patrol-update.md, U3).
//
// Take the checkpoints of the zone in position order, and for each count how many times this person
// scanned it in this zone on this work date. Then:
//   - the first checkpoint can always be scanned: a new round can start at any time;
//   - any other can be scanned only when the one before it was scanned more often than it was
//     (counts[p-1] > counts[p]): the round in progress has passed the one before.
//
// With three checkpoints 1,2,3,1,2,3 is fine, 1,3 is not (3 needs a 2), 1,2,2 is not (the second 2 needs
// a second 1) and 1,2,1,2,3 is (a round that was restarted). Another guard's scans do not count, nor
// do the scans of the same checkpoint in another zone. A declined scan still counts: a decline is the
// DANRU's verdict on a visit, it does not undo the walk.
//
// ⚠ Only the active checkpoints of the zone take part. An inactive one keeps its position (so the
// admin's order survives switching it off) but nobody can scan it, so the rule steps over it.
//
// The app is shown the result of this same function (scannable, next), and Phase 10.9's scan checks
// with it, so what the app greys out and what the server refuses cannot disagree.

// nextAllowed applies the rule. counts[i] is how many times the checkpoint at the i-th place of the
// zone was scanned. scannable[i] says whether it can be scanned now, and next is the place the guard
// should go to: the first one the current round has not reached, or the first one when the round is
// complete or has not begun. next is -1 for a zone with no checkpoints.
func nextAllowed(counts []int) (scannable []bool, next int) {
	if len(counts) == 0 {
		return nil, -1
	}
	scannable = make([]bool, len(counts))
	scannable[0] = true
	for i := 1; i < len(counts); i++ {
		scannable[i] = counts[i-1] > counts[i]
	}
	// The round in progress is the number of times the first was scanned. The first place that has
	// been scanned fewer times is where it stopped. By the rule above that place is scannable: the one
	// before it has been scanned at least as often as the first, which is more than this one.
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[0] {
			return scannable, i
		}
	}
	return scannable, 0
}

// orderStep is one active checkpoint of a zone for one person on one work date.
type orderStep struct {
	CheckpointID pgtype.UUID
	Position     int // the position in the zone, from 1 (it can skip a number when an inactive one holds it)
	Scans        int
	Scannable    bool
}

// zoneOrder is where a person stands in a zone.
type zoneOrder struct {
	Steps []orderStep
	// Next indexes Steps; -1 when the zone has no active checkpoint.
	Next int
}

// endOfTime is "before" for a read that wants every scan so far, however late the app dated it.
var endOfTime = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// loadOrder reads one person's scans of a zone on a work date, made before a moment, and applies the
// rule. A scan counts when its occurred_at is before `before`: the check of a scan passes its own
// occurred_at (so a queue synced in occurred_at order is judged as it would have been online), the
// field read passes endOfTime.
//
// ⚠ Positions are read as they are now. If an admin reorders the zone in the middle of a shift, the
// new order applies from the next scan; scans already made keep counting for the checkpoint they
// were, wherever it stands now.
func loadOrder(ctx context.Context, q *db.Queries, personnelID, zoneID int64, workDate timeutil.Date, before time.Time) (zoneOrder, error) {
	rows, err := q.CountScansByPosition(ctx, db.CountScansByPositionParams{
		PersonnelID: personnelID, ZoneID: zoneID, WorkDate: pgtype.Date{Time: workDate.Time(), Valid: true}, Before: ts(before),
	})
	if err != nil {
		return zoneOrder{}, fmt.Errorf("count scans by position: %w", err)
	}
	steps := make([]orderStep, len(rows))
	for i, r := range rows {
		steps[i] = orderStep{CheckpointID: r.CheckpointID, Position: int(r.Position), Scans: int(r.Scans)}
	}
	return orderOf(steps), nil
}

// orderOf applies the rule to steps that have their Scans, in position order, and fills Scannable and Next.
// The one place the rule meets the data, shared by the read of one zone (loadOrder) and of many
// (the bundled list), so the two cannot disagree.
func orderOf(steps []orderStep) zoneOrder {
	counts := make([]int, len(steps))
	for i, st := range steps {
		counts[i] = st.Scans
	}
	scannable, next := nextAllowed(counts)
	for i := range steps {
		steps[i].Scannable = scannable[i]
	}
	return zoneOrder{Steps: steps, Next: next}
}
