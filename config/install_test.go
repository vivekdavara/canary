// Package config_test checks the manifests in this directory with the kubectl a user would run.
package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// README, "Run it": the CRD has to be applied with --server-side. A client-side apply copies the whole object into an
// annotation, and the CRD is bigger than annotations may be.
func TestTheCRDNeedsServerSideApply(t *testing.T) {
	env := testenv.Get(t)
	_, stderr, err := env.Kubectl("apply", "-f", "crd/bases")
	if err == nil {
		t.Fatal("a client-side apply of the CRD succeeded; README says it can't (and should say otherwise now)")
	}
	if !strings.Contains(stderr, "Too long") {
		t.Errorf("the client-side apply failed, but not on the annotation's size: %s", stderr)
	}
	stdout, stderr, err := env.Kubectl("apply", "--server-side", "-f", "crd/bases")
	if err != nil {
		t.Fatalf("kubectl apply --server-side: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "canaryrollouts.rollouts.vivekdavara.github.io serverside-applied") {
		t.Errorf("kubectl apply --server-side said %q", stdout)
	}
}

// The sample applies with a plain kubectl apply, and kubectl get shows the CRD's columns.
func TestTheSampleAppliesWithKubectl(t *testing.T) {
	env := testenv.Get(t)
	ns := env.Namespace(t)
	stdout, stderr, err := env.Kubectl("apply", "-n", ns, "-f", "samples/web.yaml")
	if err != nil {
		t.Fatalf("kubectl apply: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "canaryrollout.rollouts.vivekdavara.github.io/web created") {
		t.Errorf("kubectl apply said %q", stdout)
	}
	stdout, stderr, err = env.Kubectl("get", "crl", "-n", ns)
	if err != nil {
		t.Fatalf("kubectl get crl: %v: %s", err, stderr)
	}
	header := strings.Fields(strings.SplitN(stdout, "\n", 2)[0])
	want := []string{"NAME", "PHASE", "STEP", "CANARY", "%", "REVISION", "AGE"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Errorf("kubectl get crl header %q; want %q", header, want)
	}
}

// README, "Run it": config/default installs the CRD, the operator's service account with its permissions, and the
// operator itself, with one server-side apply. envtest runs no pods, so this checks the manifests the API server
// accepts and the permissions they grant; test/e2e runs them on kind.
func TestTheOperatorsManifestsApply(t *testing.T) {
	env := testenv.Get(t)
	stdout, stderr, err := env.Kubectl("apply", "--server-side", "-k", "default")
	if err != nil {
		t.Fatalf("kubectl apply --server-side -k default: %v: %s", err, stderr)
	}
	for _, obj := range []string{
		"namespace/canary-system", "customresourcedefinition.apiextensions.k8s.io/canaryrollouts.rollouts.vivekdavara.github.io",
		"serviceaccount/canary", "role.rbac.authorization.k8s.io/canary-leader-election",
		"clusterrole.rbac.authorization.k8s.io/canary-manager", "rolebinding.rbac.authorization.k8s.io/canary-leader-election",
		"clusterrolebinding.rbac.authorization.k8s.io/canary-manager", "deployment.apps/canary",
	} {
		if !strings.Contains(stdout, obj+" serverside-applied") {
			t.Errorf("kubectl apply didn't apply %s:\n%s", obj, stdout)
		}
	}

	// The service account holds the generated ClusterRole everywhere and the Lease's Role only in its own
	// namespace; nothing else.
	env.CheckOperatorPermissions(t, "canary-system:canary", "canary-system")

	// The Deployment runs the operator as that account, with leader election, both ports and the probes.
	stdout, stderr, err = env.Kubectl("get", "deployment", "canary", "-n", "canary-system", "-o",
		"jsonpath={.spec.template.spec.serviceAccountName} {.spec.template.spec.containers[0].args} {.spec.template.spec.containers[0].readinessProbe.httpGet.port}")
	if err != nil {
		t.Fatalf("kubectl get deployment: %v: %s", err, stderr)
	}
	if want := `canary ["--leader-elect","--metrics-bind-address=:8111","--health-probe-bind-address=:8121"] probes`; stdout != want {
		t.Errorf("the operator's Deployment: %s\nwant %s", stdout, want)
	}
}

// The end-to-end tests' Prometheus (test/e2e/config) is accepted by the API server as it stands; test/e2e runs it on
// kind, with the operator installed by the chart. (A server-side dry run can't check it: it creates no namespace, so
// everything in one is refused.)
func TestTheEndToEndOverlayApplies(t *testing.T) {
	env := testenv.Get(t)
	stdout, stderr, err := env.Kubectl("apply", "--server-side", "-k", "../test/e2e/config")
	if err != nil {
		t.Fatalf("kubectl apply --server-side -k ../test/e2e/config: %v: %s", err, stderr)
	}
	for _, obj := range []string{"namespace/canary-e2e", "configmap/prometheus", "deployment.apps/prometheus", "service/prometheus"} {
		if !strings.Contains(stdout, obj+" serverside-applied") {
			t.Errorf("the dry run didn't apply %s:\n%s", obj, stdout)
		}
	}
}
