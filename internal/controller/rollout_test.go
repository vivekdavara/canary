package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
	"github.com/vivekdavara/canary/internal/rollout"
	"github.com/vivekdavara/canary/internal/testenv"
)

// envtest runs no Deployment controller, so these tests play its part: markReady writes the status a Deployment
// controller would once every pod of the current spec is ready.
func markReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	for range poll(t) {
		var d appsv1.Deployment
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
			t.Fatal(err)
		}
		n := ptr.Deref(d.Spec.Replicas, 1)
		d.Status = appsv1.DeploymentStatus{
			ObservedGeneration: d.Generation,
			Replicas:           n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n,
		}
		err := c.Status().Update(context.Background(), &d)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
	}
	t.Fatalf("couldn't mark %s ready", name)
}

// deploymentWhen polls the Deployment ns/name until ok accepts it (nil when it doesn't exist).
func deploymentWhen(t *testing.T, c client.Client, ns, name string, ok func(*appsv1.Deployment) bool) *appsv1.Deployment {
	t.Helper()
	var last *appsv1.Deployment
	for range poll(t) {
		var d appsv1.Deployment
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d)
		switch {
		case apierrors.IsNotFound(err):
			last = nil
		case err != nil:
			t.Fatal(err)
		default:
			last = &d
		}
		if ok(last) {
			return last
		}
	}
	t.Fatalf("Deployment %s still not as wanted after 10s: %+v", name, last)
	return nil
}

// at says a Deployment exists with n replicas and the pod template of revision rev.
func at(rev string, n int32) func(*appsv1.Deployment) bool {
	return func(d *appsv1.Deployment) bool {
		return d != nil && ptr.Deref(d.Spec.Replicas, 1) == n && d.Spec.Template.Labels[LabelRevision] == rev
	}
}

func phase(p rolloutsv1alpha1.Phase) func(*rolloutsv1alpha1.CanaryRollout) bool {
	return func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.Phase == p }
}

// reasons lists the reasons of the Events recorded on the rollout ns/name, oldest first.
func reasons(t *testing.T, c client.Client, ns, name string) []string {
	t.Helper()
	var list eventsv1.EventList
	if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	slices.SortStableFunc(list.Items, func(a, b eventsv1.Event) int { return a.EventTime.Compare(b.EventTime.Time) })
	var out []string
	for _, e := range list.Items {
		if e.Regarding.Kind == "CanaryRollout" && e.Regarding.Name == name {
			out = append(out, e.Reason)
		}
	}
	return out
}

// TestARolloutEndToEnd follows DESIGN.md's state machine through a whole rollout against the API server, with the
// operator's own permissions: the stable on first apply, then a new image in steps of 25% and 50% of 4 pods, then the
// promotion.
func TestARolloutEndToEnd(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	stableName, canaryName := DeploymentName("web", TrackStable), DeploymentName("web", TrackCanary)

	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableRevision != "" })
	rev1 := revision.Of(&got.Spec.Template)

	// First apply: the stable, from the template, at all 4 pods; no canary.
	stable := deploymentWhen(t, c, ns, stableName, at(rev1, 4))
	if !metav1.IsControlledBy(stable, got) {
		t.Errorf("the stable's owner references %+v; want the rollout as controller", stable.OwnerReferences)
	}
	wantSelector := map[string]string{LabelRollout: "web", LabelTrack: TrackStable}
	if !maps(stable.Spec.Selector.MatchLabels, wantSelector) {
		t.Errorf("selector %v; want %v", stable.Spec.Selector.MatchLabels, wantSelector)
	}
	if l := stable.Spec.Template.Labels; l["app"] != "web" || l[LabelTrack] != TrackStable || l[LabelRollout] != "web" {
		t.Errorf("pod labels %v; want the template's and the operator's", l)
	}
	if ru := stable.Spec.Strategy.RollingUpdate; ru == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("strategy %+v; want maxUnavailable 0", stable.Spec.Strategy)
	}
	if got.Status.Phase != rolloutsv1alpha1.PhaseHealthy || got.Status.StableRevision != rev1 {
		t.Errorf("status %+v; want Healthy at %s", got.Status, rev1)
	}
	markReady(t, c, ns, stableName)
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableReplicas == 4 })
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAvailable); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Available = %+v; want True", cond)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != rollout.ReasonIdle {
		t.Errorf("Progressing = %+v; want False, Idle", cond)
	}

	// A new image: step 1 (25% of 4) runs one canary pod, and the stable keeps its 4 until that pod is ready.
	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhaseProgressing))
	rev2 := revision.Of(&got.Spec.Template)
	deploymentWhen(t, c, ns, canaryName, at(rev2, 1))
	deploymentWhen(t, c, ns, stableName, at(rev1, 4))
	if got.Status.CanaryRevision != rev2 || ptr.Deref(got.Status.CurrentStep, -1) != 0 {
		t.Errorf("status %+v; want step 0 of %s", got.Status, rev2)
	}

	// Its pod ready (pause 0): step 2 (50%) at once, two canary pods.
	markReady(t, c, ns, canaryName)
	deploymentWhen(t, c, ns, canaryName, at(rev2, 2))
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return ptr.Deref(r.Status.CurrentStep, -1) == 1 })

	// Those ready: the last step passed. The stable takes the new template at 4 pods; the canary keeps its 2.
	markReady(t, c, ns, canaryName)
	deploymentWhen(t, c, ns, stableName, at(rev2, 4))
	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhasePromoting))
	if d := deploymentWhen(t, c, ns, canaryName, func(d *appsv1.Deployment) bool { return d != nil }); ptr.Deref(d.Spec.Replicas, 0) != 2 {
		t.Errorf("canary at %d pods during the promotion; want its 2 kept", ptr.Deref(d.Spec.Replicas, 0))
	}

	// The stable's new pods ready: the canary goes to 0, and the rollout is Healthy at the new revision.
	markReady(t, c, ns, stableName)
	deploymentWhen(t, c, ns, canaryName, at(rev2, 0))
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		return r.Status.Phase == rolloutsv1alpha1.PhaseHealthy && r.Status.StableRevision == rev2
	})
	if got.Status.CanaryRevision != "" || got.Status.CurrentStep != nil {
		t.Errorf("status %+v; want no rollout", got.Status)
	}
	want := []string{rollout.EventRolloutStarted, rollout.EventStepAdvanced, rollout.EventPromoting, rollout.EventPromoted}
	for range poll(t) {
		if slices.Equal(reasons(t, c, ns, "web"), want) {
			return
		}
	}
	t.Errorf("events %v; want %v", reasons(t, c, ns, "web"), want)
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A step's pause ends on a requeue, with no other event to wake the operator: the pause is real time here (2s).
func TestAPauseEndsOnItsOwn(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.Replicas = 2
	ro.Spec.Steps = []rolloutsv1alpha1.Step{{Weight: 50, Pause: &metav1.Duration{Duration: 2 * time.Second}}}
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableRevision != "" })
	markReady(t, c, ns, "web-stable")
	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	deploymentWhen(t, c, ns, "web-canary", func(d *appsv1.Deployment) bool { return d != nil })
	markReady(t, c, ns, "web-canary")
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StepStartedAt != nil })
	started := got.Status.StepStartedAt.Time
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing); cond == nil || cond.Reason != rollout.ReasonStepPaused {
		t.Errorf("Progressing = %+v; want StepPaused", cond)
	}
	deploymentWhen(t, c, ns, "web-stable", at(got.Status.StableRevision, 1))

	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhasePromoting))
	if now := time.Now(); now.Before(started.Add(2 * time.Second)) {
		t.Errorf("promoting at %v, before the pause's end %v", now, started.Add(2*time.Second))
	}
}

// A template the CRD accepts but a Deployment can't have (a label value with a "!") is reported on the rollout, with
// a Warning Event, and nothing is created; fixing it carries on.
func TestATemplateTheAPIServerRefuses(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.Template.Labels["app"] = "web!"
	if err := c.Create(ctx, ro); err != nil {
		t.Fatalf("the CRD refused the label (it doesn't check label syntax; DESIGN.md says so): %v", err)
	}
	key := client.ObjectKeyFromObject(ro)
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		cond := meta.FindStatusCondition(r.Status.Conditions, ConditionProgressing)
		return cond != nil && cond.Reason == ReasonInvalidSpec
	})
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "web-stable") || !strings.Contains(cond.Message, "web!") {
		t.Errorf("Progressing = %+v; want False with the API server's message", cond)
	}
	if got.Status.Phase != "" || got.Status.StableRevision != "" {
		t.Errorf("status %+v; want no phase or stable revision: nothing runs", got.Status)
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-stable"}, &d); !apierrors.IsNotFound(err) {
		t.Errorf("get web-stable: %v; want not found", err)
	}
	for range poll(t) {
		if slices.Contains(reasons(t, c, ns, "web"), ReasonInvalidSpec) {
			break
		}
	}
	if !slices.Contains(reasons(t, c, ns, "web"), ReasonInvalidSpec) {
		t.Errorf("events %v; want an InvalidSpec Warning", reasons(t, c, ns, "web"))
	}

	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) { r.Spec.Template.Labels["app"] = "web" })
	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhaseHealthy))
	deploymentWhen(t, c, ns, "web-stable", at(got.Status.StableRevision, 4))
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing); cond.Reason != rollout.ReasonIdle {
		t.Errorf("Progressing = %+v; want Idle after the fix", cond)
	}
}

// A Deployment named web-stable that the rollout doesn't control is left alone and reported; once it's gone, the
// rollout creates its own.
func TestSomeoneElsesDeploymentWithTheName(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	theirs := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-stable"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](7),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "theirs"}},
			Template: newRollout(ns, "example.com/theirs:1").Spec.Template,
		},
	}
	theirs.Spec.Template.Labels = map[string]string{"app": "theirs"}
	if err := c.Create(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		cond := meta.FindStatusCondition(r.Status.Conditions, ConditionProgressing)
		return cond != nil && cond.Reason == ReasonDeploymentConflict
	})
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing); !strings.Contains(cond.Message, "web-stable") {
		t.Errorf("message %q; want it to name the Deployment", cond.Message)
	}
	if d := deploymentWhen(t, c, ns, "web-stable", func(d *appsv1.Deployment) bool { return d != nil }); ptr.Deref(d.Spec.Replicas, 0) != 7 || len(d.OwnerReferences) != 0 {
		t.Errorf("their Deployment was changed: %+v", d)
	}

	if err := c.Delete(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	// Deleting it is an event for a Deployment the rollout doesn't own, so nothing wakes the operator for this
	// rollout; a change to the rollout does.
	edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) { r.Annotations = map[string]string{"poke": "1"} })
	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhaseHealthy))
	deploymentWhen(t, c, ns, "web-stable", at(got.Status.StableRevision, 4))
}
