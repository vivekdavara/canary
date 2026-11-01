package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/vivekdavara/canary/internal/testenv"
)

// Review (day 2): newDeployment sets the owner reference with controllerutil.SetControllerReference, which sets
// blockOwnerDeletion: true. On an API server with the OwnerReferencesPermissionEnforcement admission plugin
// (OpenShift enables it; it is a standard plugin, off by default), creating an object with a blocking owner
// reference needs `update` on the owner's `finalizers` subresource, which the generated role lacks
// (kubebuilder's scaffold adds a canaryrollouts/finalizers marker for exactly this). Every Deployment create is then
// Forbidden: no rollout ever gets its stable. The control sub-test adds only that one rule and passes.
//
// It starts an API server of its own (with the plugin), so it costs a few seconds.
func TestReviewD2RoleIsEnoughWithOwnerReferencesPermissionEnforcement(t *testing.T) {
	env := testenv.Get(t) // skips without envtest, like the rest of the package
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	te.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	cfg, err := te.Start()
	t.Cleanup(func() {
		if err := te.Stop(); err != nil {
			t.Errorf("stopping the second API server: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var generated rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(data, &generated); err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct {
		name  string
		extra []rbacv1.PolicyRule
	}{
		{"the generated role", nil},
		{"control: plus update on canaryrollouts/finalizers", []rbacv1.PolicyRule{{
			APIGroups: []string{"rollouts.vivekdavara.github.io"}, Resources: []string{"canaryrollouts/finalizers"}, Verbs: []string{"update"},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			user := "review-operator-" + string(rune('a'+i))
			role := generated.DeepCopy()
			role.Name = user
			role.Rules = append(role.Rules, tc.extra...)
			binding := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: user},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: user},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}
			for _, obj := range []client.Object{role, binding} {
				if err := admin.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}
			au, err := te.AddUser(envtest.User{Name: user}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			mgr, err := ctrl.NewManager(au.Config(), ctrl.Options{
				Scheme:                 env.Scheme,
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				Controller:             config.Controller{SkipNameValidation: ptr.To(true), CacheSyncTimeout: 10 * time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := New(mgr).SetupWithManager(mgr); err != nil {
				t.Fatal(err)
			}
			mctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- mgr.Start(mctx) }()
			t.Cleanup(func() { cancel(); <-done })

			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
			if err := admin.Create(ctx, nsObj); err != nil {
				t.Fatal(err)
			}
			ro := newRollout(nsObj.Name, "example.com/web:1")
			if err := admin.Create(ctx, ro); err != nil {
				t.Fatal(err)
			}
			for range poll(t) {
				var d appsv1.Deployment
				err := admin.Get(ctx, client.ObjectKey{Namespace: nsObj.Name, Name: "web-stable"}, &d)
				if err == nil {
					return
				}
				if !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			}
			t.Errorf("no web-stable after 10s with %s: the operator's Deployment create is refused (blockOwnerDeletion "+
				"needs update on canaryrollouts/finalizers under OwnerReferencesPermissionEnforcement)", tc.name)
		})
	}
}
