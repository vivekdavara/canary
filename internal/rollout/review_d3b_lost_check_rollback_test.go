package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of day 3, second round. a005015 says a rollback whose status write was lost no longer lets a deleted stable
// make the failed revision the stable, but it only covers the deadline (and a lowered limit): a rollback decided by a
// failed check whose status write is lost leaves failedChecks at what it was, so alreadyFailed sees nothing, and the
// day-2 rule recreates the stable from the failed template at all replicas, phase Healthy, without the check that was
// due (and failed) ever being run again.
func TestReviewD3bADeletedStableAfterALostFailedCheckRollback(t *testing.T) {
	w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Second, 0).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run() // the pause starts: stable 3, canary 1
	w.now = w.state.StepStartedAt.Add(time.Second)
	last := w.state
	w.run() // the first check fails: the rollback, the stable back to 4...
	if w.state.Phase != v1.PhaseRolledBack || w.stable.Replicas != 4 {
		t.Fatalf("state %+v, stable %+v; want the rollback", w.state, w.stable)
	}
	w.state = last // ...but the status write is lost (a conflict, or the operator stopped)
	w.stable = nil // and the stable is deleted before the next reconcile
	w.now = w.now.Add(100 * time.Millisecond)
	checked := w.checked
	w.run()
	if w.stable != nil && w.stable.Revision == "r2" {
		t.Fatalf("the failed revision r2 became the stable at %d pods (phase %s), and the due check ran %d times; want no stable from r2",
			w.stable.Replicas, w.state.Phase, w.checked-checked)
	}
}
