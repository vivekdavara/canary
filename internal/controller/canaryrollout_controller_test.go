package controller

import (
	"context"
	"iter"
	"os"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
	"github.com/vivekdavara/canary/internal/revision"
	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// startOperator runs the reconciler in a manager of the test's own, as the operator's user (only the generated
// ClusterRole's permissions), until the test ends. It returns an administrator's client for the test to act with.
func startOperator(t *testing.T) client.Client {
	t.Helper()
	runOperator(t)
	return testenv.Get(t).Client
}

// runOperator starts the reconciler as startOperator does and returns a function that stops it and waits until it
// has (the test's end calls it too; a second call does nothing).
func runOperator(t *testing.T) (stop func()) {
	t.Helper()
	return runOperatorWithClock(t, nil)
}

// runOperatorWithClock is runOperator with the reconciler's clock set (time.Now when nil).
func runOperatorWithClock(t *testing.T, now func() time.Time) (stop func()) {
	t.Helper()
	return runOperatorWith(t, operatorOptions{now: now})
}

// operatorOptions change how a test's operator runs.
type operatorOptions struct {
	now   func() time.Time   // the reconciler's clock; time.Now when nil
	funcs *interceptor.Funcs // when set, the manager's client goes through these (to count or fail calls)
	// querier answers the analysis's queries (nil: none, as without --prometheus-url), each bounded by
	// analysisTimeout (the default when 0).
	querier         analysis.Querier
	analysisTimeout time.Duration
}

func runOperatorWith(t *testing.T, opts operatorOptions) (stop func()) {
	t.Helper()
	env := testenv.Get(t)
	var newClient client.NewClientFunc
	if opts.funcs != nil {
		newClient = func(cfg *rest.Config, o client.Options) (client.Client, error) {
			wc, err := client.NewWithWatch(cfg, o)
			if err != nil {
				return nil, err
			}
			return interceptor.NewClient(wc, *opts.funcs), nil
		}
	}
	mgr, err := ctrl.NewManager(env.OperatorConfig(t), ctrl.Options{
		NewClient:              newClient,
		Scheme:                 env.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller: config.Controller{
			// Each test starts a manager, and controller names must otherwise be unique in a process.
			SkipNameValidation: ptr.To(true),
			// A permission missing from the role shows up as a cache that never syncs: fail in seconds, not minutes.
			CacheSyncTimeout: 10 * time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(mgr)
	r.Now, r.Querier, r.AnalysisTimeout = opts.now, opts.querier, opts.analysisTimeout
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("the manager stopped with %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func newRollout(ns, image string) *rolloutsv1alpha1.CanaryRollout {
	return &rolloutsv1alpha1.CanaryRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: rolloutsv1alpha1.CanaryRolloutSpec{
			Replicas: 4,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "web", Image: image, Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				}}},
			},
			Steps: []rolloutsv1alpha1.Step{{Weight: 25}, {Weight: 50}},
		},
	}
}

// poll yields every 50 ms for up to 10 seconds: a loop over it that hasn't returned by then has timed out.
func poll(t *testing.T) iter.Seq[int] {
	return func(yield func(int) bool) {
		deadline := time.Now().Add(10 * time.Second)
		for i := 0; time.Now().Before(deadline); i++ {
			if !yield(i) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// edit changes the rollout at key with change and updates it, reading it again and retrying when the update
// conflicts (the operator writes the status meanwhile), and returns it as updated.
func edit(t *testing.T, c client.Client, key client.ObjectKey, change func(*rolloutsv1alpha1.CanaryRollout)) *rolloutsv1alpha1.CanaryRollout {
	t.Helper()
	var ro rolloutsv1alpha1.CanaryRollout
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(context.Background(), key, &ro); err != nil {
			return err
		}
		change(&ro)
		return c.Update(context.Background(), &ro)
	})
	if err != nil {
		t.Fatal(err)
	}
	return &ro
}

// eventually polls get until ok says it's done, for up to 10 seconds.
func eventually(t *testing.T, c client.Client, key client.ObjectKey, ok func(*rolloutsv1alpha1.CanaryRollout) bool) *rolloutsv1alpha1.CanaryRollout {
	t.Helper()
	var ro rolloutsv1alpha1.CanaryRollout
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := c.Get(context.Background(), key, &ro)
		if err == nil && ok(&ro) {
			return &ro
		}
		if time.Now().After(deadline) {
			t.Fatalf("still not there after 10s: err %v, status %+v", err, ro.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStatusFollowsTheSpec(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)

	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	// The revision is the stored template's (the port's protocol defaulted), not the sent one's.
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 1 })
	if want := revision.Of(&got.Spec.Template); got.Status.UpdateRevision != want {
		t.Errorf("updateRevision = %q; want %q, the stored template's", got.Status.UpdateRevision, want)
	}
	first := got.Status.UpdateRevision

	// A new image: a new generation and a new revision.
	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 2 })
	if want := revision.Of(&got.Spec.Template); got.Status.UpdateRevision != want || want == first {
		t.Errorf("after the image change: updateRevision %q, first %q; want %q, a new one", got.Status.UpdateRevision, first, want)
	}

	// A spec change that leaves the template alone moves the generation but not the revision.
	second := got.Status.UpdateRevision
	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) { r.Spec.Replicas = 6 })
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 3 })
	if got.Status.UpdateRevision != second {
		t.Errorf("after a replicas change: updateRevision %q; want %q kept", got.Status.UpdateRevision, second)
	}
}

// The reconciler writes only its own fields: what else is in the status stays (rolledBackRevision is day 3's).
func TestReconcileKeepsTheRestOfTheStatus(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)

	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 1 })
	// The operator may write the status meanwhile (its Deployment's events bring reconciles): read again on a conflict.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur rolloutsv1alpha1.CanaryRollout
		if err := c.Get(ctx, key, &cur); err != nil {
			return err
		}
		cur.Status.RolledBackRevision = "0123456789abcdef"
		return c.Status().Update(ctx, &cur)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got *rolloutsv1alpha1.CanaryRollout
	got = edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 2 })
	if got.Status.RolledBackRevision != "0123456789abcdef" {
		t.Errorf("rolledBackRevision = %q; want it kept", got.Status.RolledBackRevision)
	}
}

// A rollout deleted before its reconcile is no error.
func TestReconcileOfAMissingRollout(t *testing.T) {
	env := testenv.Get(t)
	r := &CanaryRolloutReconciler{Client: env.Client, APIReader: env.Client, Recorder: events.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "gone"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Errorf("Reconcile = %+v, %v; want an empty result and no error", res, err)
	}
}
