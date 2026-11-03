package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// Review of b6b6df8 (round 4): tests for the repair's survivors. They pass on b6b6df8; each kills a mutant that every
// test of b6b6df8 lets through.

// reviewD4Stored reads lastCheckAt as stored.
func reviewD4Stored(t *testing.T, ro *rolloutsv1alpha1.CanaryRollout) (string, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	if err := testenv.Get(t).Client.Get(context.Background(), client.ObjectKeyFromObject(ro), u); err != nil {
		t.Fatal(err)
	}
	s, found, _ := unstructured.NestedString(u.Object, "status", "analysis", "lastCheckAt")
	return s, found
}

// Six digits with a lower-case "z" doesn't decode as a MicroTime either, so it must be repaired. Kills the mutant
// that upper-cases before the early "already in microseconds" check (the rollout would be InvalidSpec for good).
func TestReviewD4ALowerCaseMicroTimeIsRepaired(t *testing.T) {
	r, ro, _ := reviewD3cStore(t, "2026-11-02t10:00:02.123456z")
	reconcileOnce(t, r, ro)
	if s, _ := reviewD4Stored(t, ro); s != "2026-11-02T10:00:02.123456Z" {
		t.Fatalf("lastCheckAt %q after the repair; want 2026-11-02T10:00:02.123456Z", s)
	}
}

// A rewrite the API server refuses (here an interceptor refuses the first patch as Invalid) removes the field, and
// the reconcile ends without error. Kills the mutant without the fallback (b6b6df8's 9999 test no longer reaches it:
// keeping the offset makes that rewrite valid).
func TestReviewD4ARefusedRewriteRemovesTheTime(t *testing.T) {
	r0, ro, _ := reviewD3cStore(t, "2026-11-02T10:00:02Z")
	refused := false
	r := direct(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !refused {
				refused = true
				return apierrors.NewInvalid(schema.GroupKind{Group: rolloutsv1alpha1.GroupVersion.Group, Kind: "CanaryRollout"}, obj.GetName(), nil)
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.Now = r0.Now
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if s, found := reviewD4Stored(t, ro); found {
		t.Fatalf("lastCheckAt %q after a refused rewrite; want it removed", s)
	}
}

// A transient failure of the rewrite (503) is an error, retried with backoff; the instant isn't thrown away. Kills
// the mutant that removes the field after any error.
func TestReviewD4ATransientRepairFailureKeepsTheTime(t *testing.T) {
	r0, ro, _ := reviewD3cStore(t, "2026-11-02T10:00:02Z")
	failed := false
	r := direct(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !failed {
				failed = true
				return apierrors.NewServiceUnavailable("try again")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.Now = r0.Now
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)}); err == nil {
		t.Fatal("Reconcile: no error after the repair's patch failed with 503; want it retried")
	}
	if s, _ := reviewD4Stored(t, ro); s != "2026-11-02T10:00:02Z" {
		t.Fatalf("lastCheckAt %q after a transient failure; want it unchanged", s)
	}
	reconcileOnce(t, r, ro)
	if s, _ := reviewD4Stored(t, ro); s != "2026-11-02T10:00:02.000000Z" {
		t.Fatalf("lastCheckAt %q after the retry; want 2026-11-02T10:00:02.000000Z", s)
	}
}
