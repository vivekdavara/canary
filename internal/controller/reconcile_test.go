package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// direct is a reconciler run by hand, without a manager: its client talks to the API server directly (no cache),
// through funcs.
func direct(t *testing.T, funcs interceptor.Funcs) *CanaryRolloutReconciler {
	t.Helper()
	env := testenv.Get(t)
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return &CanaryRolloutReconciler{
		Client:    interceptor.NewClient(wc, funcs),
		APIReader: env.Client,
		Recorder:  events.NewFakeRecorder(100),
	}
}

func reconcileOnce(t *testing.T, r *CanaryRolloutReconciler, ro *rolloutsv1alpha1.CanaryRollout) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// The status patch carries the resourceVersion read, so a status computed from a spec that changed meanwhile is
// refused, and the reconcile ends without error (the change brings its own). Here the spec changes between the read
// and the patch.
func TestAStatusFromAnOlderSpecIsNotWritten(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	ro := newRollout(env.Namespace(t), "example.com/web:1")
	if err := env.Client.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	var patches atomic.Int32
	r := direct(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches.Add(1)
			var cur rolloutsv1alpha1.CanaryRollout
			if err := env.Client.Get(ctx, client.ObjectKeyFromObject(obj), &cur); err != nil {
				return err
			}
			cur.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
			if err := env.Client.Update(ctx, &cur); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	reconcileOnce(t, r, ro)
	if patches.Load() != 1 {
		t.Fatalf("%d status patches; want 1", patches.Load())
	}
	var got rolloutsv1alpha1.CanaryRollout
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ro), &got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != 2 || got.Status.ObservedGeneration != 0 || got.Status.Phase != "" {
		t.Errorf("generation %d, status %+v; want generation 2 and nothing written", got.Generation, got.Status)
	}
}

// Once everything matches, a reconcile writes nothing: no Deployment update, no status patch, no Event.
func TestAReconcileWithNothingNewWritesNothing(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	ns := env.Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	if err := env.Client.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	count := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes.Add(1)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes.Add(1)
			return c.Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			writes.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
	r := direct(t, count)
	reconcileOnce(t, r, ro) // creates the stable, writes the status
	markReady(t, env.Client, ns, "web-stable")
	reconcileOnce(t, r, ro) // records the ready pods
	if writes.Load() < 3 {
		t.Fatalf("%d writes so far; want the create and two status patches", writes.Load())
	}
	recorder := r.Recorder.(*events.FakeRecorder)
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}

	writes.Store(0)
	for range 3 {
		reconcileOnce(t, r, ro)
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("%d writes from reconciles with nothing new; want 0", n)
	}
	if len(recorder.Events) != 0 {
		t.Errorf("%d Events from reconciles with nothing new; want 0", len(recorder.Events))
	}
}

// The operator keeps no state of its own: a new operator process (here, a second manager after the first stopped)
// carries on a rollout where the last one left it, keeping the step's start time. Each operator has a fixed clock,
// the second ten minutes after the first, so a start time reset by the restart would differ (status times are stored
// to the second, and the whole test takes less than one).
func TestANewOperatorCarriesOn(t *testing.T) {
	first := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	stop := runOperatorWithClock(t, func() time.Time { return first })
	c := testenv.Get(t).Client
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.Steps = []rolloutsv1alpha1.Step{{Weight: 50, Pause: &metav1.Duration{Duration: time.Hour}}}
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableRevision != "" })
	markReady(t, c, ns, "web-stable")
	edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	deploymentWhen(t, c, ns, "web-canary", func(d *appsv1.Deployment) bool { return d != nil })
	markReady(t, c, ns, "web-canary")
	paused := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StepStartedAt != nil })
	if !paused.Status.StepStartedAt.Time.Equal(first) {
		t.Fatalf("the first operator recorded stepStartedAt %v; want its clock's %v", paused.Status.StepStartedAt, first)
	}
	deploymentWhen(t, c, ns, "web-stable", func(d *appsv1.Deployment) bool { return ptr.Deref(d.Spec.Replicas, 0) == 2 })
	stop()

	var canary appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-canary"}, &canary); err != nil {
		t.Fatal(err)
	}
	runOperatorWithClock(t, func() time.Time { return first.Add(10 * time.Minute) })
	// The pause doubled: the new operator reconciles (a new generation) and keeps the step's start time, so the pause
	// still ends two hours after the old operator saw the pods ready, not two hours after the restart.
	doubled := edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Steps[0].Pause = &metav1.Duration{Duration: 2 * time.Hour}
	})
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == doubled.Generation })
	if !got.Status.StepStartedAt.Time.Equal(first) || got.Status.Phase != rolloutsv1alpha1.PhaseProgressing {
		t.Errorf("status %+v; want still paused, stepStartedAt %v kept across operators", got.Status, first)
	}
	end := first.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing); cond == nil || !strings.Contains(cond.Message, end) {
		t.Errorf("Progressing = %+v; want paused until %s", cond, end)
	}
	var after appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-canary"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != canary.ResourceVersion {
		t.Errorf("the canary Deployment was rewritten by the new operator (resourceVersion %s, was %s)", after.ResourceVersion, canary.ResourceVersion)
	}
}

// Events go out only once the status write that records their transition succeeds: a reconcile whose write loses a
// conflict sends none, and the next one, deciding again from the same status, sends them once.
func TestEventsOnlyAfterTheStatusIsWritten(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	ns := env.Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	if err := env.Client.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	var conflict atomic.Bool
	r := direct(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if conflict.Load() {
				return apierrors.NewConflict(rolloutsv1alpha1.GroupVersion.WithResource("canaryrollouts").GroupResource(), obj.GetName(), errors.New("injected"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	recorder := r.Recorder.(*events.FakeRecorder)
	reconcileOnce(t, r, ro)
	markReady(t, env.Client, ns, "web-stable")
	reconcileOnce(t, r, ro)
	edit(t, env.Client, client.ObjectKeyFromObject(ro), func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})

	conflict.Store(true)
	reconcileOnce(t, r, ro)
	if n := len(recorder.Events); n != 0 {
		t.Fatalf("%d Events after a status write that lost its conflict; want none", n)
	}
	conflict.Store(false)
	reconcileOnce(t, r, ro)
	reconcileOnce(t, r, ro)
	var got []string
	for len(recorder.Events) > 0 {
		got = append(got, <-recorder.Events)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "Normal RolloutStarted ") {
		t.Errorf("Events %q; want one RolloutStarted", got)
	}
}

// An undecodable rollout's report is written once (a second reconcile finds it there and writes nothing), and with
// the resourceVersion it read, so a rollout changed meanwhile isn't reported from the older read.
func TestAnUndecodableRolloutsReport(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	key := func(ns string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: "web"} }
	unstructuredAt := func(k client.ObjectKey) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
		if err := env.Client.Get(ctx, k, u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	request := func(ns string) *rolloutsv1alpha1.CanaryRollout {
		return &rolloutsv1alpha1.CanaryRollout{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}}
	}

	t.Run("written once", func(t *testing.T) {
		ns := env.Namespace(t)
		createRaw(t, ns, undecodable(t, ns))
		var patches atomic.Int32
		r := direct(t, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				patches.Add(1)
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		reconcileOnce(t, r, request(ns))
		reconcileOnce(t, r, request(ns))
		if n := patches.Load(); n != 1 {
			t.Errorf("%d status patches for two reconciles of the same undecodable rollout; want 1", n)
		}
		if st, _ := unstructuredAt(key(ns)).Object["status"].(map[string]any); st == nil || st["observedGeneration"] == nil {
			t.Errorf("status %v; want the report", st)
		}
	})

	t.Run("locked", func(t *testing.T) {
		ns := env.Namespace(t)
		createRaw(t, ns, undecodable(t, ns))
		r := direct(t, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				// Someone changes the rollout between the operator's read and its write.
				u := unstructuredAt(key(ns))
				u.SetAnnotations(map[string]string{"changed": "meanwhile"})
				if err := env.Client.Update(ctx, u); err != nil {
					return err
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		reconcileOnce(t, r, request(ns))
		if st := unstructuredAt(key(ns)).Object["status"]; st != nil {
			t.Errorf("status %v written over a rollout that changed after it was read; want none", st)
		}
	})
}
