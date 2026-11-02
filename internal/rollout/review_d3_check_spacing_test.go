package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of day 3. A check's time is recorded rounded down to the second, and the next is due an interval after
// that, so the next check can come up to a second sooner than an interval after the check that ran. With a 1s
// interval two checks can run back to back: here a check that ran 0.9s late (start+1.9s, recorded as start+1s) and
// a reconcile 100 ms later (any event: a Deployment's status, the status write's own watch event) runs a second one.
// One bad moment is then measured twice, and with failureLimit 1 it rolls the rollout back, though only one interval
// (one second) began since the pause did... and only 0.1s passed between the two measurements.
func TestReviewD3ChecksAreAnIntervalApart(t *testing.T) {
	w := newWorld(t, 2, step(50, time.Hour)).withAnalysis(time.Second, 1).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run() // the pause starts
	start := w.state.StepStartedAt.Time

	w.now = start.Add(1900 * time.Millisecond)
	w.run() // the first check, 0.9s late: one failure, tolerated
	if w.checked != 1 || w.state.Phase != v1.PhaseProgressing {
		t.Fatalf("%d checks, state %+v; want one failure tolerated", w.checked, w.state)
	}
	w.now = start.Add(2000 * time.Millisecond)
	w.run() // 100 ms later
	if w.checked != 1 || w.state.Phase != v1.PhaseProgressing {
		t.Fatalf("a second check ran 100 ms after the first (interval 1s): %d checks, phase %s, analysis %+v",
			w.checked, w.state.Phase, w.state.Analysis)
	}
}
