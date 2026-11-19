package patrol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNextAllowed(t *testing.T) {
	for name, tc := range map[string]struct {
		counts    []int
		scannable []bool
		next      int
	}{
		"no checkpoint":                     {nil, nil, -1},
		"one checkpoint is always open":     {[]int{0}, []bool{true}, 0},
		"one checkpoint, scanned often":     {[]int{5}, []bool{true}, 0},
		"nothing done: only the first":      {[]int{0, 0, 0}, []bool{true, false, false}, 0},
		"the first is done":                 {[]int{1, 0, 0}, []bool{true, true, false}, 1},
		"two are done":                      {[]int{1, 1, 0}, []bool{true, false, true}, 2},
		"a round is done: a new one starts": {[]int{1, 1, 1}, []bool{true, false, false}, 0},
		"a round restarted":                 {[]int{2, 1, 0}, []bool{true, true, true}, 1},
		"the second round, two in":          {[]int{2, 2, 1}, []bool{true, false, true}, 2},
		"three rounds done":                 {[]int{3, 3, 3}, []bool{true, false, false}, 0},
		"two checkpoints, restarted":        {[]int{2, 1}, []bool{true, true}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			scannable, next := nextAllowed(tc.counts)
			require.Equal(t, tc.scannable, scannable)
			require.Equal(t, tc.next, next)
		})
	}
}

// walk applies the checkpoints a guard scans, by position from 1, and says at which step the rule
// first refuses one (0 when it never does), with the counts at that moment.
func walk(n int, steps []int) (refusedAt int) {
	counts := make([]int, n)
	for i, p := range steps {
		scannable, _ := nextAllowed(counts)
		if !scannable[p-1] {
			return i + 1
		}
		counts[p-1]++
	}
	return 0
}

// The examples of the plan (docs/plan/10.1-patrol-update.md, "The order rule"), with three checkpoints.
func TestOrderRuleExamples(t *testing.T) {
	for name, tc := range map[string]struct {
		steps     []int
		refusedAt int
	}{
		"1,2,3,1,2,3 is two rounds":             {[]int{1, 2, 3, 1, 2, 3}, 0},
		"1,3 skips the 2":                       {[]int{1, 3}, 2},
		"1,2,2 needs a second 1 for a second 2": {[]int{1, 2, 2}, 3},
		"1,2,1,2,3 restarts a round":            {[]int{1, 2, 1, 2, 3}, 0},
		"starting at 2":                         {[]int{2}, 1},
		"starting at 3":                         {[]int{3}, 1},
		"1,1,1: a new round may begin at once":  {[]int{1, 1, 1}, 0},
		"1,2,3,3":                               {[]int{1, 2, 3, 3}, 4},
		"1,2,3,1,3 after a restart":             {[]int{1, 2, 3, 1, 3}, 5},
		"1,2,3,2 the round is over":             {[]int{1, 2, 3, 2}, 4},
		"nothing at all":                        {nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.refusedAt, walk(3, tc.steps))
		})
	}
	t.Run("one checkpoint is never refused", func(t *testing.T) {
		require.Zero(t, walk(1, []int{1, 1, 1, 1}))
	})
}

// The place `next` names is always one the guard may scan, whatever walk led to it: the app is told to
// go there, so the rule must not refuse it. Every walk of up to eight valid scans over three and four
// checkpoints is tried.
func TestNextIsAlwaysScannable(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4} {
		var visit func(counts []int, depth int)
		visit = func(counts []int, depth int) {
			scannable, next := nextAllowed(counts)
			require.GreaterOrEqual(t, next, 0)
			require.True(t, scannable[next], fmt.Sprintf("n=%d counts=%v next=%d", n, counts, next))
			if depth == 8 {
				return
			}
			for p := range counts {
				if !scannable[p] {
					continue
				}
				c := append([]int(nil), counts...)
				c[p]++
				visit(c, depth+1)
			}
		}
		visit(make([]int, n), 0)
	}
}
