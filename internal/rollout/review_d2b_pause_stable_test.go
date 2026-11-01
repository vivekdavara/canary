package rollout

import (
	"testing"
	"time"
)

// Review d2b: "a pause belongs to the pods it started with" is checked on the canary's side only. A replicas edit
// that keeps the step's canary count (10 -> 9 at 50%: canary 5 either way) keeps the pause, and the pause plan
// scales the stable straight down to the new split (5 -> 4) without the capacity rules, even when a canary pod is
// not ready. Ready pods go from 9 to 8 with spec.replicas 9: below min(replicas, ready before), the walk's own
// invariant. The walk can't find it because its simulated Deployments never lose a ready pod.
func TestReviewD2bReplicasCutMidPauseWithACanaryPodDown(t *testing.T) {
	w := newWorld(t, 10, step(50, time.Minute), step(100, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run() // step 0: canary to 5, stable kept at 10
	w.settle()
	w.run() // canary ready at 5: the pause starts, stable to 5
	w.settle()
	if w.state.StepStartedAt == nil || w.pods() != [2]int32{5, 5} {
		t.Fatalf("setup: state %+v, pods %v; want a running pause at 5/5", w.state, w.pods())
	}
	w.now = w.now.Add(10 * time.Second)
	// One canary pod stops being ready mid-pause (it must not restart the pause), then replicas go to 9.
	w.canary.ReadyReplicas, w.canary.Settled = 4, false
	w.spec.Replicas = 9
	before := readyOf(w.stable) + readyOf(w.canary)
	p := w.run()
	after := readyOf(w.stable) + readyOf(w.canary)
	if after < min(w.spec.Replicas, before) {
		t.Fatalf("ready pods %d -> %d with replicas %d: the pause plan scaled the stable to %d while only %d of the canary's %d pods were ready (reason %s)",
			before, after, w.spec.Replicas, p.Stable.Replicas, w.canary.ReadyReplicas, w.canary.Replicas, p.Reason)
	}
}
