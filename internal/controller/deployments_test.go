package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
}
