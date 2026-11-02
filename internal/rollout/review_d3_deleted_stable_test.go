package rollout

import (
	"testing"
	"time"
)

// Review of day 3. Day 3 added the rule that a deleted stable is not recreated from a rolled-back template ("that
// would run the failed version at all replicas, with no canary and no analysis"), but only once the RolledBack status
// is written. The stable==nil branch runs before any rollback check, so a rollout whose deadline has passed (or whose
// rollback decision's status write was lost, as here) and whose stable is then deleted makes the failed revision the
// stable at all replicas, phase Healthy, instead of rolling back.
func TestReviewD3ADeletedStableAfterALostRollbackWrite(t *testing.T) {
	w := newWorld(t, 4, step(25, 0)).withDeadline(60).started()
	w.now = w.now.Add(time.Minute) // the canary's pod was never ready: the deadline is past
	last := w.state
	w.run()        // the rollback: the stable goes back to 4 pods...
	w.state = last // ...but the status write is lost (a conflict)
	w.stable = nil // and the stable is deleted before the next reconcile
	w.run()
	if w.stable != nil && w.stable.Revision == "r2" {
		t.Fatalf("the failed revision r2 became the stable at %d pods (phase %s, %+v); want it rolled back, not run at all replicas",
			w.stable.Replicas, w.state.Phase, w.state)
	}
}
