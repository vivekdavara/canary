package rollout

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of day 3: random walks with every edit TestRandomWalksWithAnalysis doesn't make (steps, interval, deadline,
// analysis removed and added, either Deployment deleted, the template set back to a rolled-back revision), status
// times truncated to the second as the API server stores them, and invariants the existing walk doesn't check:
//   - a timed plan (WaitingForCanary, StepPaused) asks to be requeued (RequeueAfter > 0);
//   - a rolled-back revision is never started again until the template changes, and while held the stable is never
//     created or given the rolled-back revision;
//   - a written decision emits one AnalysisFailed per failed check it recorded;
//   - capacity, as in the other walks.
func TestReviewD3Walks(t *testing.T) {
	census := map[string]int{}
	for seed := range uint64(6000) {
		rng := rand.New(rand.NewPCG(seed, 33))
		w := newWorld(t, int32(1+rng.IntN(12)), randomSteps(rng)...).
			withDeadline(int32(30+rng.IntN(300))).
			withAnalysis(time.Duration(1+rng.IntN(90))*time.Second, int32(rng.IntN(3)))
		passRate := []float64{1, 0.9, 0.5, 0.1}[rng.IntN(4)]
		w.checks = func() analysis.Check {
			if rng.Float64() < passRate {
				return passing
			}
			return failing
		}
		w.run()
		w.settle()
		revs := 1
		heldRev := "" // a revision rolled back (as written) and the template unchanged since
		for i := range 120 {
			lost := rng.IntN(6) == 0
			switch rng.IntN(20) {
			case 0, 1, 2, 3, 4:
				if w.stable != nil || w.canary != nil {
					if w.stable == nil || (w.canary != nil && rng.IntN(2) == 0) {
						w.canary.Settled, w.canary.ReadyReplicas = true, w.canary.Replicas
					} else {
						w.stable.Settled, w.stable.ReadyReplicas = true, w.stable.Replicas
					}
				}
			case 5, 6, 7:
				w.now = w.now.Add(time.Duration(rng.IntN(120000)) * time.Millisecond)
			case 8:
				revs++
				w.rev = fmt.Sprintf("r%d", revs)
				heldRev = ""
				census["new template"]++
			case 9:
				if w.state.RolledBackRevision != "" && w.rev != w.state.RolledBackRevision {
					w.rev = w.state.RolledBackRevision
					heldRev = ""
					census["template back to a rolled-back revision"]++
				} else if w.stable != nil && w.rev != w.stable.Revision {
					w.rev = w.stable.Revision
					heldRev = ""
				}
			case 10:
				w.spec.Replicas = int32(1 + rng.IntN(12))
				census["replicas changed"]++
			case 11:
				w.spec.Steps = randomSteps(rng)
				census["steps changed"]++
			case 12:
				if w.spec.Analysis != nil {
					w.spec.Analysis.Interval = &metav1.Duration{Duration: time.Duration(1+rng.IntN(90)) * time.Second}
					census["interval changed"]++
				}
			case 13:
				w.spec.ProgressDeadlineSeconds = ptr(int32(1 + rng.IntN(300)))
				census["deadline changed"]++
			case 14:
				if w.spec.Analysis != nil && rng.IntN(3) == 0 {
					w.spec.Analysis = nil
					census["analysis removed"]++
				} else if w.spec.Analysis == nil {
					w.withAnalysis(time.Duration(1+rng.IntN(90))*time.Second, int32(rng.IntN(3)))
					census["analysis added"]++
				} else {
					w.spec.Analysis.FailureLimit = ptr(int32(rng.IntN(3)))
				}
			case 15:
				if d := []*Deployment{w.stable, w.canary}[rng.IntN(2)]; d != nil && d.ReadyReplicas > 0 {
					d.ReadyReplicas--
					d.Settled = false
					census["ready pod lost"]++
				}
			case 16:
				if rng.IntN(4) == 0 {
					if rng.IntN(2) == 0 {
						w.stable = nil
						census["stable deleted"]++
					} else {
						w.canary = nil
						census["canary deleted"]++
					}
				}
			}
			heldRev = w.reviewStep(seed, i, lost, heldRev, census)
		}
		w.checks = nil
		for range 10 * (len(w.spec.Steps) + 2) {
			w.settle()
			w.now = w.now.Add(3 * time.Minute)
			heldRev = w.reviewStep(seed, -1, false, heldRev, census)
		}
		r := w.spec.Replicas
		switch {
		case w.state.Phase == v1.PhaseHealthy && w.stable != nil && w.stable.Revision == w.rev && replicasOf(w.canary) == 0 && w.stable.Replicas == r:
			census["ended Healthy"]++
		case w.state.Phase == v1.PhaseRolledBack && w.state.RolledBackRevision == w.rev && w.stable != nil && w.stable.Revision != w.rev &&
			replicasOf(w.canary) == 0 && w.stable.Replicas == r:
			census["ended rolled back"]++
		case w.state.Phase == v1.PhaseRolledBack && w.state.RolledBackRevision == w.rev && w.stable == nil:
			census["ended rolled back, stable deleted"]++
		default:
			t.Fatalf("seed %d: ended %+v, stable %+v, canary %+v; want Healthy at %s or holding after its rollback", seed, w.state, w.stable, w.canary, w.rev)
		}
	}
	t.Logf("census: %v", census)
}

// truncateTimes is the status as the API server stores it: times to the second, lastCheckAt (a MicroTime) to the
// microsecond.
func truncateTimes(s State) State {
	tr := func(t *metav1.Time) *metav1.Time {
		if t == nil {
			return nil
		}
		return &metav1.Time{Time: t.Truncate(time.Second)}
	}
	s.StepStartedAt, s.StepWaitingSince = tr(s.StepStartedAt), tr(s.StepWaitingSince)
	if t := s.Analysis.LastCheckAt; t != nil {
		s.Analysis.LastCheckAt = &metav1.MicroTime{Time: t.Truncate(time.Microsecond)}
	}
	return s
}

func (w *world) reviewStep(seed uint64, i int, lost bool, heldRev string, census map[string]int) string {
	w.t.Helper()
	in := w.state
	stableBefore, canaryBefore := copyOf(w.stable), copyOf(w.canary)
	fail := func(format string, args ...any) {
		w.t.Helper()
		w.t.Fatalf("seed %d step %d: %s\n  before: %+v\n    stable %+v, canary %+v\n  after: %+v\n    stable %+v, canary %+v\n  template %s, replicas %d, steps %+v, analysis %+v, deadline %v",
			seed, i, fmt.Sprintf(format, args...), in, stableBefore, canaryBefore, w.state, w.stable, w.canary, w.rev, w.spec.Replicas, w.spec.Steps, w.spec.Analysis, *w.spec.ProgressDeadlineSeconds)
	}
	before := readyOf(w.stable) + readyOf(w.canary)
	p := w.run()

	if after := readyOf(w.stable) + readyOf(w.canary); after < min(w.spec.Replicas, before) {
		fail("a decision took the ready pods from %d to %d (replicas %d)", before, after, w.spec.Replicas)
	}
	if (p.Reason == ReasonWaitingForCanary || p.Reason == ReasonStepPaused) && p.RequeueAfter <= 0 {
		fail("a timed plan (%s) with RequeueAfter %v: nothing wakes the operator", p.Reason, p.RequeueAfter)
	}
	if heldRev != "" && w.rev == heldRev {
		census["decided while held"]++
		for _, e := range p.Events {
			if e.Reason == EventRolloutStarted {
				fail("the rolled-back revision %s started again with the template unchanged", heldRev)
			}
		}
		if w.stable != nil && (stableBefore == nil || w.stable.Revision == heldRev) {
			fail("while held, the stable was created or took the rolled-back revision")
		}
	}
	failedEvents := 0
	for _, e := range p.Events {
		if e.Reason == EventAnalysisFailed {
			failedEvents++
		}
	}
	if p.Checked != nil && !p.Checked.Passed && failedEvents != 1 {
		fail("a failed check with %d AnalysisFailed Events", failedEvents)
	}
	if lost {
		census["status write lost"]++
		w.state = in
		return heldRev
	}
	w.state = truncateTimes(w.state)
	if w.state.Phase == v1.PhaseRolledBack && in.Phase != v1.PhaseRolledBack {
		census["rolled back"]++
		heldRev = w.state.RolledBackRevision
	}
	return heldRev
}
