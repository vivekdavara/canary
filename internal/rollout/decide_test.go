package rollout

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// world is a rollout and its two Deployments, with a Deployment controller simulated by hand: run applies a plan as
// the operator would, and settle makes both Deployments' pods ready, as Kubernetes would some time later.
type world struct {
	t              *testing.T
	spec           v1.CanaryRolloutSpec
	rev            string
	state          State
	stable, canary *Deployment
	now            time.Time
	events         []string       // reasons, in order
	edited         bool           // the rollout in progress was edited (TestRandomWalksWithEdits)
	pauses         map[int32]bool // the steps whose pause started in the rollout in progress, as written
}

func newWorld(t *testing.T, replicas int32, steps ...v1.Step) *world {
	return &world{
		t:    t,
		spec: v1.CanaryRolloutSpec{Replicas: replicas, Steps: steps},
		rev:  "r1",
		now:  time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}
}

func step(weight int32, pause time.Duration) v1.Step {
	return v1.Step{Weight: weight, Pause: &metav1.Duration{Duration: pause}}
}

// run decides once and applies the plan: the status is written, and each Deployment gets its target.
func (w *world) run() Plan {
	w.t.Helper()
	p := Decide(Input{Spec: &w.spec, Revision: w.rev, Status: w.state, Stable: w.stable, Canary: w.canary, Now: w.now})
	w.state = p.State
	w.stable = apply(w.stable, p.Stable, w.rev)
	w.canary = apply(w.canary, p.Canary, w.rev)
	for _, e := range p.Events {
		w.events = append(w.events, e.Reason)
	}
	return p
}

func apply(d *Deployment, t Target, rev string) *Deployment {
	switch {
	case d == nil && !t.FromTemplate:
		return nil
	case d == nil:
		return &Deployment{Revision: rev, Replicas: t.Replicas}
	}
	next := *d
	// A template change replaces pods one at a time, a new one ready before an old one goes (the operator's
	// Deployments don't let pods be unavailable), so the ready count holds; scaling down removes ready pods at once.
	if t.FromTemplate && d.Revision != rev {
		next.Revision, next.Settled = rev, false
	}
	if d.Replicas != t.Replicas {
		next.Replicas, next.Settled = t.Replicas, false
		next.ReadyReplicas = min(next.ReadyReplicas, t.Replicas)
	}
	return &next
}

// settle makes every pod of both Deployments ready.
func (w *world) settle() {
	for _, d := range []*Deployment{w.stable, w.canary} {
		if d != nil {
			d.Settled, d.ReadyReplicas = true, d.Replicas
		}
	}
}

// pods is "stable/canary" spec replicas, with the canary 0 when it doesn't exist.
func (w *world) pods() [2]int32 {
	return [2]int32{replicasOf(w.stable), replicasOf(w.canary)}
}

func (w *world) expect(phase v1.Phase, stable, canary int32) {
	w.t.Helper()
	if w.state.Phase != phase || w.pods() != [2]int32{stable, canary} {
		w.t.Fatalf("phase %s, pods (stable, canary) %v; want %s, [%d %d]", w.state.Phase, w.pods(), phase, stable, canary)
	}
}

func TestFirstApplyCreatesTheStableOnly(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, 0))
	p := w.run()
	if !p.Stable.FromTemplate || p.Stable.Replicas != 4 || p.Canary != (Target{}) {
		t.Fatalf("plan %+v; want the stable from the template at 4, no canary", p)
	}
	w.expect(v1.PhaseHealthy, 4, 0)
	if w.canary != nil || w.state.StableRevision != "r1" || p.Progressing() {
		t.Fatalf("canary %v, state %+v, progressing %v", w.canary, w.state, p.Progressing())
	}
	// Nothing changes once the pods are ready, and nothing is timed.
	w.settle()
	p = w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	if p.RequeueAfter != 0 || len(w.events) != 0 {
		t.Fatalf("requeue %v, events %v; want none", p.RequeueAfter, w.events)
	}
}

// A whole rollout of 4 pods in steps of 25% and 50%: the canary scales up first, the stable comes down only once the
// step's canary pods are ready, and the promotion keeps the canary until the stable's new pods are ready.
func TestARolloutFromStartToPromotion(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, 0))
	w.run()
	w.settle()

	w.rev = "r2"
	w.run()
	w.expect(v1.PhaseProgressing, 4, 1) // step 1: the canary's pod first, the stable keeps 4
	if w.canary.Revision != "r2" || w.stable.Revision != "r1" || *w.state.CurrentStep != 0 {
		t.Fatalf("canary %+v, stable %+v, state %+v", w.canary, w.stable, w.state)
	}
	w.run()
	w.expect(v1.PhaseProgressing, 4, 1) // still waiting: nothing is ready

	w.settle()
	// Ready with a 0s pause: step 2 begins in the same decision, so the stable keeps its 4 (step 1's 3 never
	// happens) until step 2's canary pods are ready.
	w.run()
	w.expect(v1.PhaseProgressing, 4, 2)
	if *w.state.CurrentStep != 1 || w.state.StepStartedAt != nil {
		t.Fatalf("state %+v; want step 1, not started", w.state)
	}

	w.settle()
	w.run() // the last step passed: promotion; the stable takes the template at 4
	w.expect(v1.PhasePromoting, 4, 2)
	if w.stable.Revision != "r2" || w.state.StableRevision != "r2" || w.state.CanaryRevision != "r2" || w.state.StepStartedAt != nil || *w.state.CurrentStep != 1 {
		t.Fatalf("stable %+v, state %+v; want the stable at r2", w.stable, w.state)
	}
	w.run()
	w.expect(v1.PhasePromoting, 4, 2) // the canary stays until the stable's pods are ready

	w.settle()
	p := w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	if w.state.CanaryRevision != "" || w.state.CurrentStep != nil || p.Progressing() {
		t.Fatalf("state %+v, progressing %v; want no rollout", w.state, p.Progressing())
	}
	want := []string{EventRolloutStarted, EventStepAdvanced, EventPromoting, EventPromoted}
	if !slices.Equal(w.events, want) {
		t.Fatalf("events %v; want %v", w.events, want)
	}
}

func TestPausesRunFromWhenTheStepsPodsAreReady(t *testing.T) {
	w := newWorld(t, 10, step(10, 2*time.Minute), step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()

	w.now = w.now.Add(time.Hour) // time waiting for pods doesn't count
	w.settle()
	ready := w.now
	p := w.run()
	if p.Reason != ReasonStepPaused || p.RequeueAfter != 2*time.Minute || !w.state.StepStartedAt.Time.Equal(ready) {
		t.Fatalf("plan %+v, state %+v; want paused for 2m from %v", p, w.state, ready)
	}
	w.expect(v1.PhaseProgressing, 9, 1)

	w.now = ready.Add(90 * time.Second)
	if p = w.run(); p.RequeueAfter != 30*time.Second || p.Reason != ReasonStepPaused {
		t.Fatalf("plan %+v; want 30s left", p)
	}
	w.now = ready.Add(2*time.Minute - time.Nanosecond)
	if p = w.run(); p.RequeueAfter != time.Nanosecond {
		t.Fatalf("requeue %v; want 1ns left", p.RequeueAfter)
	}
	w.now = ready.Add(2 * time.Minute)
	w.run()
	w.expect(v1.PhaseProgressing, 9, 5)
	if *w.state.CurrentStep != 1 {
		t.Fatalf("state %+v; want step 1", w.state)
	}
}

// A new template mid-rollout restarts at step 0 with it. The stable comes back up to step 0's count at once; the
// canary keeps its pods, of the old version, until the stable has them, and only then goes down to step 0's count
// with the new version.
func TestANewTemplateMidRolloutRestartsAtStepZero(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.run()
	w.settle()
	w.run() // step 1's pause
	w.expect(v1.PhaseProgressing, 2, 2)

	w.rev = "r3"
	p := w.run()
	w.expect(v1.PhaseProgressing, 3, 2)
	if w.canary.Revision != "r2" || w.stable.Revision != "r1" || *w.state.CurrentStep != 0 || w.state.StepStartedAt != nil {
		t.Fatalf("canary %+v, stable %+v, state %+v; want the canary held at r2", w.canary, w.stable, w.state)
	}
	if len(p.Events) != 1 || p.Events[0].Reason != EventRolloutStarted || !strings.Contains(p.Events[0].Message, "replacing revision r2") {
		t.Fatalf("events %+v", p.Events)
	}
	if p = w.run(); len(p.Events) != 0 || !strings.Contains(p.Message, "before the canary goes down to 1") {
		t.Fatalf("plan %+v; want waiting for the stable, no new event", p)
	}
	w.settle()
	w.run()
	w.expect(v1.PhaseProgressing, 3, 1)
	if w.canary.Revision != "r3" {
		t.Fatalf("canary %+v; want r3 now", w.canary)
	}
}

// The template going back to the stable's version ends the rollout; the canary keeps its pods until the stable is
// back at all replicas.
func TestTheTemplateBackToTheStableEndsTheRollout(t *testing.T) {
	w := newWorld(t, 4, step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.run()
	w.expect(v1.PhaseProgressing, 2, 2)

	w.rev = "r1"
	p := w.run()
	w.expect(v1.PhaseHealthy, 4, 2)
	if len(p.Events) != 1 || p.Events[0].Reason != EventRolloutReverted {
		t.Fatalf("events %+v; want RolloutReverted", p.Events)
	}
	if p = w.run(); len(p.Events) != 0 {
		t.Fatalf("events %+v; want it reported once", p.Events)
	}
	w.expect(v1.PhaseHealthy, 4, 2)
	w.settle()
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
}

// A template change after the promotion has given the stable the canary's version starts a rollout over that
// version, not the one before it.
func TestANewTemplateDuringPromotion(t *testing.T) {
	w := newWorld(t, 4, step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.run()
	w.expect(v1.PhasePromoting, 4, 2)

	w.rev = "r3"
	w.run()
	w.expect(v1.PhaseProgressing, 4, 2)
	if w.state.StableRevision != "r2" || w.canary.Revision != "r3" {
		t.Fatalf("state %+v, canary %+v; want r3 over the stable's r2", w.state, w.canary)
	}
}

func TestADeletedStableIsRecreatedFromTheTemplate(t *testing.T) {
	w := newWorld(t, 4, step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.run()
	w.stable = nil
	w.run()
	// The canary's pods may be the only ones serving: they stay until the new stable's are ready.
	w.expect(v1.PhaseHealthy, 4, 2)
	if w.stable.Revision != "r2" || w.state.CanaryRevision != "" {
		t.Fatalf("stable %+v, state %+v", w.stable, w.state)
	}
	w.run()
	w.expect(v1.PhaseHealthy, 4, 2)
	w.settle()
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
}

// Steps aren't part of the template, so they can shrink mid-rollout: the step index stays in range.
func TestFewerStepsMidRollout(t *testing.T) {
	w := newWorld(t, 10, step(10, 0), step(20, 0), step(30, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	for range 3 {
		w.run()
		w.settle()
	}
	if *w.state.CurrentStep != 2 {
		t.Fatalf("state %+v; want step 2", w.state)
	}
	w.spec.Steps = w.spec.Steps[:1]
	p := w.run()
	if *w.state.CurrentStep != 0 || p.Reason != ReasonWaitingForCanary {
		t.Fatalf("state %+v, plan %+v; want step 0, waiting for its pods", w.state, p)
	}
	w.expect(v1.PhaseProgressing, 10, 1)
	w.settle()
	w.run() // step 0 is now the last
	w.expect(v1.PhasePromoting, 10, 1)
}

// One pod: every step below 100% runs a canary pod beside the stable one, and a 100% step leaves the stable none.
func TestOnePod(t *testing.T) {
	w := newWorld(t, 1, step(50, 0), step(100, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.expect(v1.PhaseProgressing, 1, 1)
	w.settle()
	w.run()
	w.expect(v1.PhaseProgressing, 1, 1) // step 2: the canary's count is unchanged, the stable not yet down
	w.settle()
	w.run()
	w.expect(v1.PhasePromoting, 1, 1)
	w.settle()
	w.run()
	w.expect(v1.PhaseHealthy, 1, 0)
}

// Random walks: decisions interleaved with pods becoming ready, time passing, new templates (from Healthy) and the
// template going back to the stable's version (any time). After every decision:
//   - capacity: the ready pods of both Deployments are at least spec.replicas once the stable first had them all;
//   - the status agrees with itself (Healthy has no canary revision or step; a rollout's canary revision is the
//     template's);
//   - and when the walk ends, letting pods become ready and time pass always reaches Healthy at the template.
func TestRandomWalks(t *testing.T) {
	for seed := range uint64(3000) {
		rng := rand.New(rand.NewPCG(seed, 7))
		r := int32(1 + rng.IntN(12))
		var steps []v1.Step
		for w := int32(0); ; {
			w += int32(1 + rng.IntN(60))
			if w >= 100 || rng.IntN(4) == 0 {
				steps = append(steps, step(min(w, 100), time.Duration(rng.IntN(3))*time.Minute))
				break
			}
			steps = append(steps, step(w, time.Duration(rng.IntN(3))*time.Minute))
		}
		w := newWorld(t, r, steps...)
		w.run()
		w.settle()
		revs := 1
		for i := range 60 {
			switch rng.IntN(6) {
			case 0, 1:
				w.settleSome(rng)
			case 2:
				w.now = w.now.Add(time.Duration(rng.IntN(150)) * time.Second)
			case 3:
				if w.state.Phase == v1.PhaseHealthy {
					revs++
					w.rev = fmt.Sprintf("r%d", revs)
				} else if rng.IntN(4) == 0 {
					w.rev = w.stable.Revision // back to the stable's version
				}
			}
			w.run()
			w.check(seed, i)
		}
		for range 4 * (len(steps) + 2) {
			w.settle()
			w.now = w.now.Add(3 * time.Minute)
			w.run()
			w.check(seed, -1)
		}
		if w.state.Phase != v1.PhaseHealthy || w.stable.Revision != w.rev || replicasOf(w.canary) != 0 || w.stable.Replicas != r {
			t.Fatalf("seed %d: ended %+v, stable %+v, canary %+v; want Healthy at %s", seed, w.state, w.stable, w.canary, w.rev)
		}
	}
}

// settleSome makes one Deployment's pods ready (the two don't settle together in a cluster).
func (w *world) settleSome(rng *rand.Rand) {
	d := w.stable
	if rng.IntN(2) == 0 && w.canary != nil {
		d = w.canary
	}
	d.Settled, d.ReadyReplicas = true, d.Replicas
}

func (w *world) check(seed uint64, i int) {
	w.t.Helper()
	if ready := w.stable.ReadyReplicas + readyOf(w.canary); ready < w.spec.Replicas {
		w.t.Fatalf("seed %d step %d: %d ready pods, fewer than %d; stable %+v, canary %+v, state %+v", seed, i, ready, w.spec.Replicas, w.stable, w.canary, w.state)
	}
	st := w.state
	switch st.Phase {
	case v1.PhaseHealthy:
		if st.CanaryRevision != "" || st.CurrentStep != nil || st.StepStartedAt != nil || st.StableRevision != w.stable.Revision {
			w.t.Fatalf("seed %d step %d: Healthy with %+v (stable %+v)", seed, i, st, w.stable)
		}
	case v1.PhaseProgressing, v1.PhasePromoting:
		if st.CanaryRevision != w.rev || st.CurrentStep == nil || int(*st.CurrentStep) >= len(w.spec.Steps) || st.StableRevision != w.stable.Revision {
			w.t.Fatalf("seed %d step %d: %s with %+v (template %s, stable %+v)", seed, i, st.Phase, st, w.rev, w.stable)
		}
	default:
		w.t.Fatalf("seed %d step %d: phase %q", seed, i, st.Phase)
	}
}

func readyOf(d *Deployment) int32 {
	if d == nil {
		return 0
	}
	return d.ReadyReplicas
}

// The operator writes the Deployments, then the status. If the status write fails after the promotion gave the stable
// the template, the next decision still sees a promotion (not a revert): the canary is kept, the lost Promoting Event
// is sent then, and Promoted follows.
func TestAPromotionWhoseStatusWasLost(t *testing.T) {
	w := newWorld(t, 4, step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	before := w.state
	w.run()
	w.expect(v1.PhasePromoting, 4, 2)
	w.state = before // the status write failed
	w.events = nil
	w.run()
	w.expect(v1.PhasePromoting, 4, 2)
	w.settle()
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	if want := []string{EventPromoting, EventPromoted}; !slices.Equal(w.events, want) {
		t.Fatalf("events %v; want %v", w.events, want)
	}
}

// Random walks with everything a user or the cluster can do mid-rollout: edit the steps or replicas, change the
// template at any time (a new version, back to the stable's, back to the previous one), delete either Deployment,
// lose a ready pod, and lose a status write (the Deployments written, the status not). Day 2's reviews found eight
// defects in exactly these moves, which the walks above never made. After every decision:
//   - capacity: the decision itself never takes the ready pods below min(replicas, what was ready before it);
//   - a pause runs only for a canary that ran the template's revision when the decision was made;
//   - in a rollout nobody edited (no step, replicas or template change, no deletion), no step's pause (of more than
//     0s) is started twice, as recorded in the status (round 2 found a lost write at a step advance doing that). A
//     step with a 0s pause can run twice: when the one decision that starts and ends it loses its status write, the
//     next one doesn't know it passed (README, limitations);
//   - when its status was written, the status agrees with itself and with the Deployments;
//   - and when the walk ends, pods becoming ready and time passing always reach Healthy at the template.
func TestRandomWalksWithEdits(t *testing.T) {
	census := map[string]int{}
	for seed := range uint64(5000) {
		rng := rand.New(rand.NewPCG(seed, 11))
		w := newWorld(t, int32(1+rng.IntN(12)), randomSteps(rng)...)
		w.run()
		w.settle()
		revs, prev := 1, ""
		// Half the walks make no edits (new templates only from Healthy), so the "no step twice" check, which only
		// holds for rollouts nobody edited, has rollouts to check.
		edits := seed%2 == 0
		for i := range 80 {
			// A lost status write is independent of the move, so it can be the write of the decision that applied a
			// template change, a step advance or a deletion.
			lost, edited := rng.IntN(8) == 0, true
			if lost {
				census["status write lost"]++
			}
			move := rng.IntN(13)
			if !edits && move >= 5 && move <= 8 && !(move == 7 && w.state.Phase == v1.PhaseHealthy) {
				move = 0
			}
			switch move {
			case 0, 1, 2:
				w.settleSome(rng)
				edited = false
			case 3, 4:
				w.now = w.now.Add(time.Duration(rng.IntN(150)) * time.Second)
				edited = false
			case 5:
				census["steps edited"]++
				w.spec.Steps = randomSteps(rng)
			case 6:
				census["replicas changed"]++
				w.spec.Replicas = int32(1 + rng.IntN(12))
			case 7:
				switch rng.IntN(3) {
				case 0:
					revs++
					prev, w.rev = w.rev, fmt.Sprintf("r%d", revs)
				case 1:
					prev, w.rev = w.rev, w.stable.Revision
				default:
					if prev != "" {
						prev, w.rev = w.rev, prev
					}
				}
				if w.state.Phase != v1.PhaseHealthy {
					census["template changed mid-rollout"]++
				} else {
					edited = false // a new rollout, not an edit of one
				}
			case 8:
				if rng.IntN(2) == 0 {
					census["stable deleted"]++
					w.stable = nil
				} else if w.canary != nil {
					census["canary deleted"]++
					w.canary = nil
				}
			case 10:
				edited = false
				if d := []*Deployment{w.stable, w.canary}[rng.IntN(2)]; d != nil && d.ReadyReplicas > 0 {
					census["ready pod lost"]++
					d.ReadyReplicas--
					d.Settled = false
				}
			default:
				edited = false
			}
			w.step(seed, i, lost, edited)
		}
		for range 6 * (len(w.spec.Steps) + 2) {
			w.settle()
			w.now = w.now.Add(3 * time.Minute)
			w.step(seed, -1, false, false)
		}
		r := w.spec.Replicas
		if w.state.Phase != v1.PhaseHealthy || w.stable.Revision != w.rev || replicasOf(w.canary) != 0 || w.stable.Replicas != r {
			t.Fatalf("seed %d: ended %+v, stable %+v, canary %+v; want Healthy at %s with %d pods", seed, w.state, w.stable, w.canary, w.rev, r)
		}
	}
	// The walks must actually make the moves they're for, and the no-edit ones must check rollouts.
	census["pause starts checked in unedited rollouts"] = uneditedChecks
	for _, move := range []string{"steps edited", "replicas changed", "template changed mid-rollout", "stable deleted", "canary deleted", "status write lost", "ready pod lost", "pause starts checked in unedited rollouts"} {
		if census[move] < 1000 {
			t.Errorf("%q happened %d times in the walks; want at least 1000", move, census[move])
		}
	}
	t.Logf("moves: %v", census)
}

// uneditedChecks counts the pause starts the "no pause twice" check applied to (TestRandomWalksWithEdits).
var uneditedChecks int

// randomSteps is 1 to 4 strictly increasing weights with pauses of 0 to 2 minutes.
func randomSteps(rng *rand.Rand) []v1.Step {
	var steps []v1.Step
	for w := int32(0); ; {
		w += int32(1 + rng.IntN(60))
		steps = append(steps, step(min(w, 100), time.Duration(rng.IntN(3))*time.Minute))
		if w >= 100 || len(steps) == 4 || rng.IntN(3) == 0 {
			return steps
		}
	}
}

// step decides once and applies the plan, checking capacity; with lost, the status write is dropped (the Deployments
// were written first), otherwise the status is checked too. edited says the move before it was an edit, which makes
// the rollout in progress an edited one (w.edited, until it ends).
func (w *world) step(seed uint64, i int, lost, edited bool) {
	w.t.Helper()
	before := readyOf(w.stable) + readyOf(w.canary)
	last := w.state
	canaryBefore := w.canary
	if canaryBefore != nil {
		c := *canaryBefore
		canaryBefore = &c
	}
	w.edited = w.edited || edited
	w.run()
	if after := readyOf(w.stable) + readyOf(w.canary); after < min(w.spec.Replicas, before) {
		w.t.Fatalf("seed %d step %d: a decision took the ready pods from %d to %d (replicas %d); stable %+v, canary %+v, state %+v, steps %+v",
			seed, i, before, after, w.spec.Replicas, w.stable, w.canary, w.state, w.spec.Steps)
	}
	if w.state.StepStartedAt != nil && (canaryBefore == nil || canaryBefore.Revision != w.rev) {
		w.t.Fatalf("seed %d step %d: a pause runs (%+v) for a canary that ran %+v when the decision was made, not %s", seed, i, w.state, canaryBefore, w.rev)
	}
	inRollout := func(s State) bool { return s.Phase == v1.PhaseProgressing && s.CanaryRevision == w.rev }
	if lost {
		w.state = last
		return
	}
	if w.state.CanaryRevision != last.CanaryRevision || w.state.Phase == v1.PhaseHealthy {
		w.pauses = nil // a new rollout, or none
	}
	if inRollout(w.state) && last.StepStartedAt == nil && w.state.StepStartedAt != nil && pauseOf(w.spec.Steps[*w.state.CurrentStep]) > 0 {
		if w.pauses == nil {
			w.pauses = map[int32]bool{}
		}
		if !w.edited {
			uneditedChecks++
			if w.pauses[*w.state.CurrentStep] {
				w.t.Fatalf("seed %d step %d: in a rollout nobody edited, step %d's pause started a second time (%+v)", seed, i, *w.state.CurrentStep, w.state)
			}
		}
		w.pauses[*w.state.CurrentStep] = true
	}
	if w.state.Phase == v1.PhaseHealthy {
		w.edited = false
	}
	st := w.state
	switch st.Phase {
	case v1.PhaseHealthy:
		if st.CanaryRevision != "" || st.CurrentStep != nil || st.StepStartedAt != nil || st.StableRevision != w.stable.Revision {
			w.t.Fatalf("seed %d step %d: Healthy with %+v (stable %+v)", seed, i, st, w.stable)
		}
	case v1.PhaseProgressing, v1.PhasePromoting:
		if st.CanaryRevision != w.rev || st.CurrentStep == nil || int(*st.CurrentStep) >= len(w.spec.Steps) || st.StableRevision != w.stable.Revision {
			w.t.Fatalf("seed %d step %d: %s with %+v (template %s, stable %+v, %d steps)", seed, i, st.Phase, st, w.rev, w.stable, len(w.spec.Steps))
		}
		if st.StepStartedAt != nil && (w.canary == nil || w.canary.Revision != w.rev) {
			w.t.Fatalf("seed %d step %d: a pause running with the canary at %+v, not %s", seed, i, w.canary, w.rev)
		}
	default:
		w.t.Fatalf("seed %d step %d: phase %q", seed, i, st.Phase)
	}
}
