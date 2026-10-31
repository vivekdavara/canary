package controller

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// startOperator runs the reconciler in a manager of the test's own, as the operator's user (only the generated
// ClusterRole's permissions), until the test ends. It returns an administrator's client for the test to act with.
func startOperator(t *testing.T) client.Client {
	t.Helper()
	env := testenv.Get(t)
	mgr, err := ctrl.NewManager(env.OperatorConfig(t), ctrl.Options{
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
	if err := (&CanaryRolloutReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the manager stopped with %v", err)
		}
	})
	return env.Client
}

func rollout(ns, image string) *rolloutsv1alpha1.CanaryRollout {
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

	ro := rollout(ns, "example.com/web:1")
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
	got.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 2 })
	if want := revision.Of(&got.Spec.Template); got.Status.UpdateRevision != want || want == first {
		t.Errorf("after the image change: updateRevision %q, first %q; want %q, a new one", got.Status.UpdateRevision, first, want)
	}

	// A spec change that leaves the template alone moves the generation but not the revision.
	second := got.Status.UpdateRevision
	got.Spec.Replicas = 6
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 3 })
	if got.Status.UpdateRevision != second {
		t.Errorf("after a replicas change: updateRevision %q; want %q kept", got.Status.UpdateRevision, second)
	}
}

// The reconciler writes only its own fields: what else is in the status stays.
func TestReconcileKeepsTheRestOfTheStatus(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)

	ro := rollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 1 })
	got.Status.StableRevision = "0123456789abcdef"
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 2 })
	if got.Status.StableRevision != "0123456789abcdef" {
		t.Errorf("stableRevision = %q; want it kept", got.Status.StableRevision)
	}
}

// A rollout deleted before its reconcile is no error.
func TestReconcileOfAMissingRollout(t *testing.T) {
	env := testenv.Get(t)
	r := &CanaryRolloutReconciler{Client: env.Client}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "gone"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Errorf("Reconcile = %+v, %v; want an empty result and no error", res, err)
	}
}
