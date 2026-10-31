// Package testenv runs a package's tests against envtest: a real kube-apiserver and etcd with the CanaryRollout CRD
// installed, and none of Kubernetes' own controllers (DESIGN.md, "Testing").
//
// Without KUBEBUILDER_ASSETS (a plain `go test ./...`) the tests that need it skip; with CANARY_REQUIRE_ENVTEST set
// (make test, CI) the package fails instead, so a check can't pass by skipping them.
package testenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// Env is a running API server and a client for it. Config and Client are an administrator's.
type Env struct {
	Config *rest.Config
	Scheme *kruntime.Scheme
	Client client.Client

	te           *envtest.Environment
	operatorOnce sync.Once
	operator     *envtest.AuthenticatedUser
	operatorErr  error
}

var current *Env

// Main starts the API server, runs the package's tests and stops it. Call it from TestMain: os.Exit(testenv.Main(m)).
func Main(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CANARY_REQUIRE_ENVTEST") != "" {
			fmt.Fprintln(os.Stderr, "testenv: CANARY_REQUIRE_ENVTEST is set but KUBEBUILDER_ASSETS isn't (make test sets both)")
			return 1
		}
		return m.Run()
	}
	scheme := kruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		return 1
	}
	if err := rolloutsv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		return 1
	}
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	// Start installs the CRD and waits until it is served, so a CRD the API server refuses (a CEL rule over its
	// cost budget, say) fails here.
	cfg, err := te.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv: starting the API server:", err)
		return 1
	}
	code := run(m, te, cfg, scheme)
	if err := te.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "testenv: stopping the API server:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func run(m *testing.M, te *envtest.Environment, cfg *rest.Config, scheme *kruntime.Scheme) int {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv: client:", err)
		return 1
	}
	current = &Env{Config: cfg, Scheme: scheme, Client: c, te: te}
	return m.Run()
}

// OperatorConfig is a configuration for a user holding exactly the operator's ClusterRole, config/rbac/role.yaml
// (generated from the reconciler's rbac markers), so an operator run with it fails where a permission is missing,
// as it would in a cluster. envtest's API server authorizes by RBAC; the administrator's Config bypasses it.
func (e *Env) OperatorConfig(t testing.TB) *rest.Config {
	t.Helper()
	return rest.CopyConfig(e.operatorUser(t).Config())
}

// OperatorKubeconfig writes a kubeconfig for the same user into the test's temporary directory and returns its path,
// for running the operator's binary.
func (e *Env) OperatorKubeconfig(t testing.TB) string {
	t.Helper()
	data, err := e.operatorUser(t).KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (e *Env) operatorUser(t testing.TB) *envtest.AuthenticatedUser {
	t.Helper()
	e.operatorOnce.Do(func() { e.operator, e.operatorErr = e.addOperator() })
	if e.operatorErr != nil {
		t.Fatalf("setting up the operator's user: %v", e.operatorErr)
	}
	return e.operator
}

// operatorUser is the name the operator's user authenticates as.
const operatorUser = "canary-operator"

// BindLeaderElection gives the operator's user config/rbac/leader_election_role.yaml in namespace ns, as a cluster
// would where the operator runs with --leader-elect.
func (e *Env) BindLeaderElection(t testing.TB, ns string) {
	t.Helper()
	var role rbacv1.Role
	if err := readYAML(filepath.Join("config", "rbac", "leader_election_role.yaml"), &role); err != nil {
		t.Fatal(err)
	}
	role.Namespace = ns
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: operatorUser}},
	}
	for _, obj := range []client.Object{&role, binding} {
		if err := e.Client.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

// readYAML decodes a file under the repository's root, refusing unknown fields.
func readYAML(path string, into any) error {
	data, err := os.ReadFile(filepath.Join(repoRoot(), path))
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(data, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (e *Env) addOperator() (*envtest.AuthenticatedUser, error) {
	var role rbacv1.ClusterRole
	if err := readYAML(filepath.Join("config", "rbac", "role.yaml"), &role); err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := e.Client.Create(ctx, &role); err != nil {
		return nil, err
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: operatorUser}},
	}
	if err := e.Client.Create(ctx, binding); err != nil {
		return nil, err
	}
	return e.te.AddUser(envtest.User{Name: operatorUser}, e.Config)
}

// Get returns the package's Env, or skips the test when envtest isn't available.
func Get(t testing.TB) *Env {
	t.Helper()
	if current == nil {
		t.Skip("envtest isn't available: set KUBEBUILDER_ASSETS (make test does)")
	}
	return current
}

// Namespace creates a namespace of the test's own. envtest has no namespace controller, so a deleted namespace
// never finishes terminating; each test gets a fresh one instead of cleaning up.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := e.Client.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating a namespace: %v", err)
	}
	return ns.Name
}

// repoRoot is the repository's root, found from this file's path so that tests in any package find config/.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testenv: no caller information")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}
