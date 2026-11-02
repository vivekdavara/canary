package rollout

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Random walks with analysis and the progress deadline: checks that pass or fail at random (some versions are good,
// some flaky, some bad), pods that are slow or never ready, ready pods lost, status writes lost, new templates at any
// time, and replicas edits. After every decision:
//   - capacity: the decision never takes the ready pods below min(replicas, what was ready);
//   - a rollback never touches the stable's template, and while a rollout holds after one, the canary never gets
//     more pods, and the stable never takes the rolled-back revision;
//   - a step passes (or the last one promotes) only after at least one check in its pause, and with no more failed
//     checks than the limit;
//   - a written status agrees with itself and with the Deployments, RolledBack included;
//   - and when the walk ends, with pods becoming ready, time passing and checks passing, it reaches Healthy at the
//     template, or holds after a rollback of the template's revision with the stable at all replicas and no canary.
func TestRandomWalksWithAnalysis(t *testing.T) {
	census := map[string]int{}
	for seed := range uint64(4000) {
		rng := rand.New(rand.NewPCG(seed, 13))
		w := newWorld(t, int32(1+rng.IntN(12)), randomSteps(rng)...).
			withDeadline(int32(60+rng.IntN(541))).
			withAnalysis(time.Duration(10+rng.IntN(81))*time.Second, int32(rng.IntN(3)))
		passRate := []float64{1, 0.97, 0.8, 0.3}[rng.IntN(4)]
		w.checks = func() analysis.Check {
			if rng.Float64() < passRate {
				return passing
			}
			return failing
		}
		w.run()
		w.settle()
		revs := 1
		for i := range 100 {
			lost := rng.IntN(8) == 0
			switch rng.IntN(12) {
			case 0, 1, 2, 3:
				w.settleSome(rng)
			case 4, 5, 6:
				w.now = w.now.Add(time.Duration(rng.IntN(150)) * time.Second)
			case 7:
				if w.state.Phase == v1.PhaseHealthy || w.state.Phase == v1.PhaseRolledBack || rng.IntN(4) == 0 {
					revs++
					w.rev = fmt.Sprintf("r%d", revs)
					census["new template"]++
				}
			case 8:
				w.spec.Replicas = int32(1 + rng.IntN(12))
				census["replicas changed"]++
			case 9:
				if d := []*Deployment{w.stable, w.canary}[rng.IntN(2)]; d != nil && d.ReadyReplicas > 0 {
					d.ReadyReplicas--
					d.Settled = false
					census["ready pod lost"]++
				}
			}
			w.stepAnalysed(seed, i, lost, census)
		}
		w.checks = nil // checks pass from here on
		for range 8 * (len(w.spec.Steps) + 2) {
			w.settle()
			w.now = w.now.Add(3 * time.Minute)
			w.stepAnalysed(seed, -1, false, census)
		}
		r := w.spec.Replicas
		switch {
		case w.state.Phase == v1.PhaseHealthy && w.stable.Revision == w.rev && replicasOf(w.canary) == 0 && w.stable.Replicas == r:
			census["ended Healthy"]++
		case w.state.Phase == v1.PhaseRolledBack && w.state.RolledBackRevision == w.rev && w.stable.Revision != w.rev &&
			replicasOf(w.canary) == 0 && w.stable.Replicas == r:
			census["ended rolled back"]++
		default:
			t.Fatalf("seed %d: ended %+v, stable %+v, canary %+v; want Healthy at %s or holding after its rollback", seed, w.state, w.stable, w.canary, w.rev)
		}
	}
	for _, what := range []string{"rolled back by the deadline", "rolled back by the analysis", "failed check tolerated", "step passed after checks",
		"promoted", "ended Healthy", "ended rolled back", "status write lost", "lost while checking", "decided while held", "ready pod lost", "replicas changed"} {
		if census[what] < 300 {
			t.Errorf("%q happened %d times in the walks; want at least 300", what, census[what])
		}
	}
	t.Logf("census: %v", census)
}

// stepAnalysed decides once (running a check when one is due) and applies the plan, checking the invariants of
// TestRandomWalksWithAnalysis; with lost, the status write is dropped after the Deployments were written.
func (w *world) stepAnalysed(seed uint64, i int, lost bool, census map[string]int) {
	w.t.Helper()
	fail := func(format string, args ...any) {
		w.t.Helper()
		w.t.Fatalf("seed %d step %d: %s\n  before: %+v\n    stable %+v, canary %+v\n  after: %+v\n    stable %+v, canary %+v\n  template %s, replicas %d, steps %+v", seed, i,
			fmt.Sprintf(format, args...), last(w), w.before[0], w.before[1], w.state, w.stable, w.canary, w.rev, w.spec.Replicas, w.spec.Steps)
	}
	before := readyOf(w.stable) + readyOf(w.canary)
	in := w.state
	stableBefore, canaryBefore := copyOf(w.stable), copyOf(w.canary)
	w.before = [2]*Deployment{stableBefore, canaryBefore}
	w.prev = in
	checkedBefore := w.checked
	p := w.run()
	checked := w.checked > checkedBefore

	if after := readyOf(w.stable) + readyOf(w.canary); after < min(w.spec.Replicas, before) {
		fail("a decision took the ready pods from %d to %d (replicas %d)", before, after, w.spec.Replicas)
	}
	if w.state.Phase == v1.PhaseRolledBack && in.Phase != v1.PhaseRolledBack && stableBefore != nil && w.stable.Revision != stableBefore.Revision {
		fail("a rollback changed the stable's template")
	}
	if in.Phase == v1.PhaseRolledBack && w.rev == in.RolledBackRevision {
		census["decided while held"]++
		if replicasOf(w.canary) > replicasOf(canaryBefore) {
			fail("the canary got more pods while the rollout holds after a rollback")
		}
		if w.stable != nil && w.stable.Revision == w.rev {
			fail("the stable took the rolled-back revision")
		}
	}
	// A step's pause ended: a StepAdvanced, or a Promoting that isn't the recovery of a lost promotion.
	for _, e := range p.Events {
		passed := e.Reason == EventStepAdvanced ||
			(e.Reason == EventPromoting && !(stableBefore != nil && stableBefore.PromotedRevision == w.rev))
		if !passed {
			continue
		}
		checks, failed := in.Analysis.Checks, in.Analysis.FailedChecks
		if in.StepStartedAt == nil {
			checks, failed = 0, 0 // the pause began in this decision: no checks yet
		}
		if p.Checked != nil {
			checks++
			if !p.Checked.Passed {
				failed++
			}
		}
		if checks < 1 || failed > failureLimitOf(w.spec.Analysis) {
			fail("a step passed with %d checks, %d failed (limit %d)", checks, failed, failureLimitOf(w.spec.Analysis))
		}
		census["step passed after checks"]++
		if e.Reason == EventPromoting {
			census["promoted"]++
		}
	}
	for _, e := range p.Events {
		switch {
		case e.Reason == EventRolledBack && p.Checked != nil && !p.Checked.Passed:
			census["rolled back by the analysis"]++
		case e.Reason == EventRolledBack:
			census["rolled back by the deadline"]++
		case e.Reason == EventAnalysisFailed && p.State.Phase != v1.PhaseRolledBack:
			census["failed check tolerated"]++
		}
	}

	if lost {
		census["status write lost"]++
		if checked {
			census["lost while checking"]++
		}
		w.state = in
		return
	}
	st := w.state
	switch st.Phase {
	case v1.PhaseHealthy:
		if st.CanaryRevision != "" || st.CurrentStep != nil || st.StepStartedAt != nil || st.StepWaitingSince != nil ||
			st.Analysis != (Analysis{}) || st.StableRevision != w.stable.Revision {
			fail("Healthy with a rollout's fields")
		}
	case v1.PhaseProgressing, v1.PhasePromoting:
		if st.CanaryRevision != w.rev || st.CurrentStep == nil || int(*st.CurrentStep) >= len(w.spec.Steps) || st.StableRevision != w.stable.Revision {
			fail("%s inconsistent", st.Phase)
		}
		if (st.StepStartedAt == nil) == (st.StepWaitingSince == nil) && st.Phase == v1.PhaseProgressing {
			fail("a step both waiting and paused, or neither")
		}
		if st.StepStartedAt == nil && st.Analysis != (Analysis{}) {
			fail("checks counted with no pause running")
		}
		if st.Analysis.FailedChecks > failureLimitOf(w.spec.Analysis) || st.Analysis.FailedChecks > st.Analysis.Checks {
			fail("more failed checks than allowed, or than checks")
		}
	case v1.PhaseRolledBack:
		if st.RolledBackRevision == "" || st.CanaryRevision != "" || st.StepStartedAt != nil || st.StepWaitingSince != nil ||
			(w.stable != nil && (st.StableRevision != w.stable.Revision || w.stable.Revision == st.RolledBackRevision)) {
			fail("RolledBack inconsistent")
		}
	default:
		fail("phase %q", st.Phase)
	}
}

// last is the state before the walk's current decision, for failure messages.
func last(w *world) State { return w.prev }

func copyOf(d *Deployment) *Deployment {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}
