package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// A Deployment is settled only when its controller has seen the latest spec and every pod it counts is of the
// current template and ready: each part of the rule on its own (unit, no API server).
func TestObserveSettled(t *testing.T) {
	d := func(gen, observed int64, replicas, total, updated, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(replicas),
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelRevision: "r1"}}},
			},
			Status: appsv1.DeploymentStatus{ObservedGeneration: observed, Replicas: total, UpdatedReplicas: updated, ReadyReplicas: ready},
		}
	}
	for _, tc := range []struct {
		name string
		d    *appsv1.Deployment
		want bool
	}{
		{"all 4 updated and ready", d(2, 2, 4, 4, 4, 4), true},
		{"the controller hasn't seen the latest spec", d(3, 2, 4, 4, 4, 4), false},
		{"an old pod still counted (rolling update)", d(2, 2, 4, 5, 4, 4), false},
		{"one pod of the old template", d(2, 2, 4, 4, 3, 4), false},
		{"one pod not ready", d(2, 2, 4, 4, 4, 3), false},
		{"scaled to 0, none left", d(2, 2, 0, 0, 0, 0), true},
	} {
		got := observe(tc.d)
		if got.Settled != tc.want || got.Revision != "r1" || got.Replicas != ptr.Deref(tc.d.Spec.Replicas, 1) || got.ReadyReplicas != tc.d.Status.ReadyReplicas {
			t.Errorf("%s: observe = %+v; want settled %v", tc.name, got, tc.want)
		}
	}
	if observe(nil) != nil {
		t.Error("observe(nil) isn't nil")
	}
	promoted := d(1, 1, 4, 4, 4, 4)
	promoted.Annotations = map[string]string{AnnotationPromotedRevision: "r1"}
	if got := observe(promoted).PromotedRevision; got != "r1" {
		t.Errorf("PromotedRevision = %q; want the annotation's r1", got)
	}
}

// rolloutNamedFor, case by case, against the API server's rollouts (web and a-stable exist; nothing else does).
func TestRolloutNamedFor(t *testing.T) {
	env := testenv.Get(t)
	ns := env.Namespace(t)
	for _, name := range []string{"web", "a-stable"} {
		if err := env.Client.Create(context.Background(), named(newRollout(ns, "example.com/web:1"), name)); err != nil {
			t.Fatal(err)
		}
	}
	r := &CanaryRolloutReconciler{Client: env.Client}
	controlledBy := func(kind, apiVersion, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, APIVersion: apiVersion, Name: name, UID: "u", Controller: ptr.To(true)}}
	}
	gv := rolloutsv1alpha1.GroupVersion.String()
	for _, tc := range []struct {
		name   string
		owners []metav1.OwnerReference
		want   string // the rollout enqueued, "" for none
	}{
		{"web-stable", nil, "web"},
		{"web-canary", nil, "web"},
		{"web-stable", controlledBy("CanaryRollout", gv, "web"), ""},      // its own: Owns maps it
		{"web-canary", controlledBy("CanaryRollout", gv, "web"), ""},      // its own
		{"web-stable", controlledBy("CanaryRollout", gv, "other"), "web"}, // another rollout's, in web's way
		{"web-stable", controlledBy("CanaryRollout", "example.com/v1", "web"), "web"},
		{"web-stable", controlledBy("ReplicaSet", "apps/v1", "web"), "web"},
		{"web", nil, ""},
		{"web-stablex", nil, ""},
		{"nope-stable", nil, ""},             // no such rollout
		{"a-stable-canary", nil, "a-stable"}, // the canary of rollout a-stable
		{"a-stable-stable", nil, "a-stable"},
		{"a-stable", nil, ""}, // would be rollout "a", which doesn't exist
	} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tc.name, OwnerReferences: tc.owners}}
		got := r.rolloutNamedFor(context.Background(), d)
		want := []reconcile.Request{}
		if tc.want != "" {
			want = append(want, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: tc.want}})
		}
		if len(got) != len(want) || (len(got) == 1 && got[0] != want[0]) {
			t.Errorf("%s owned by %+v: %v; want %v", tc.name, tc.owners, got, want)
		}
	}
}

func named(ro *rolloutsv1alpha1.CanaryRollout, name string) *rolloutsv1alpha1.CanaryRollout {
	ro.Name = name
	return ro
}
