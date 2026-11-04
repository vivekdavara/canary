package charts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivekdavara/canary/internal/testenv"
)

// Review (day 5): podLabels is "type: object, additionalProperties: string" in the schema, and the template writes
// it after the chart's own labels. A podLabels key that is one of the selector's labels (app.kubernetes.io/name or
// app.kubernetes.io/instance) renders a duplicate YAML key; the last one wins, so the pod template no longer
// matches the Deployment's selector and the API server refuses the install. The schema (or the template) should
// refuse or ignore those keys; here the install fails at the API server instead. Fixed: the chart refuses a podLabels
// key it sets itself, before anything reaches the API server, so the assertion is now that refusal (it was "the
// install succeeds"; the reviewer's own text asks for a refusal).
func TestReviewD5PodLabelsCanOverrideTheSelector(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	for i, set := range []string{`podLabels.app\.kubernetes\.io/name=team-a`, `podLabels.app\.kubernetes\.io/instance=team-a`} {
		release := []string{"labels-a", "labels-b"}[i]
		_, stderr, err := helm(t, "install", release, "./canary", "-n", ns, "--kubeconfig", kubeconfig,
			"--set", set, "--set", "replicaCount=1", "--set", "leaderElection.enabled=false")
		t.Cleanup(func() { helm(t, "uninstall", release, "-n", ns, "--kubeconfig", kubeconfig) })
		if err == nil {
			t.Errorf("--set %s installed: the pod labels override the chart's", set)
		} else if !strings.Contains(stderr, "podLabels can't set app.kubernetes.io/") {
			t.Errorf("--set %s was refused, but not by the chart: %s", set, lastLines(stderr, 2))
		}
	}
}

// Review (day 5): imagePullSecrets is "type: array" with no item schema, so the natural
// --set imagePullSecrets[0]=regcred passes the schema and renders a list of strings, which the API server refuses
// (it wants [{name: regcred}]). Fixed: the chart takes both forms, a name or {name: ...}.
func TestReviewD5ImagePullSecretsAcceptsStrings(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	_, stderr, err := helm(t, "install", "secrets", "./canary", "-n", ns, "--kubeconfig", kubeconfig,
		"--set", "imagePullSecrets[0]=regcred", "--set", "replicaCount=1", "--set", "leaderElection.enabled=false")
	t.Cleanup(func() { helm(t, "uninstall", "secrets", "-n", ns, "--kubeconfig", kubeconfig) })
	if err != nil {
		t.Errorf("imagePullSecrets[0]=regcred passed the schema and the API server refused it: %s", lastLines(stderr, 2))
	}
}

// Review (day 5): DESIGN.md, "Installing it": "a release that changes the CRD needs
// kubectl apply --server-side -f charts/canary/crds/ before helm upgrade". Helm created the CRD (field manager
// helm), so a server-side apply by kubectl that changes any field helm set conflicts and is refused without
// --force-conflicts. Here the "new" CRD is the chart's with one description changed, as a release would. Fixed in
// the docs: the command applies as Helm's own field manager, kubectl apply --server-side --field-manager=helm, so
// nothing conflicts and Helm still owns the fields (with --force-conflicts instead, kubectl took them, and a later
// helm install of the chart conflicted with "kubectl"). This test runs the documented command (it ran the old one),
// checks the change took, and that a helm install afterwards still works.
func TestReviewD5DocumentedCRDUpgradeCommandConflicts(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	// Make sure Helm is the one that created the CRD (in this package's API server nothing else does).
	if _, stderr, err := helm(t, "install", "canary-crd", "./canary", "-n", ns, "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("helm install: %v: %s", err, stderr)
	}
	t.Cleanup(func() { helm(t, "uninstall", "canary-crd", "-n", ns, "--kubeconfig", kubeconfig) })
	crd, err := os.ReadFile("canary/crds/rollouts.vivekdavara.github.io_canaryrollouts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(crd), "description:") {
		t.Fatal("no description in the CRD")
	}
	// Change the first description line, whatever it says.
	lines := strings.Split(string(crd), "\n")
	for i, l := range lines {
		if strings.Contains(l, "description:") {
			lines[i] = l + " (changed in a later release)"
			break
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "crd.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := env.Kubectl("apply", "--server-side", "--field-manager=helm", "-f", path); err != nil {
		t.Fatalf("DESIGN.md's CRD upgrade command fails: %v: %s", err, firstLines(stderr, 3))
	}
	stdout, stderr, err := env.Kubectl("get", "crd", "canaryrollouts.rollouts.vivekdavara.github.io", "-o", "yaml")
	if err != nil {
		t.Fatalf("kubectl get crd: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "(changed in a later release)") {
		t.Error("the CRD upgrade command succeeded but the CRD didn't change")
	}
	other := env.Namespace(t)
	if _, stderr, err := helm(t, "install", "canary-after", "./canary", "-n", other, "--kubeconfig", kubeconfig,
		"--set", "replicaCount=1", "--set", "leaderElection.enabled=false"); err != nil {
		t.Errorf("helm install after the CRD upgrade: %v: %s", err, lastLines(stderr, 2))
	}
	t.Cleanup(func() { helm(t, "uninstall", "canary-after", "-n", other, "--kubeconfig", kubeconfig) })
}

// Review (day 5): DESIGN.md: "Names follow the release ... so two releases don't fight over a ClusterRole". Two
// releases named foo and foo-canary both name their ClusterRole foo-canary-manager ("foo" gets "-canary" appended,
// "foo-canary" says canary already), and so does the same release name in two namespaces; the second install is
// refused over the ClusterRole's ownership. Fixed: objects are named exactly after the release (foo, foo-canary).
// The same release name in two namespaces is still refused over the ClusterRole (DESIGN.md: one operator per
// cluster).
func TestReviewD5TwoReleasesShareAClusterRoleName(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	a, b := env.Namespace(t), env.Namespace(t)
	for _, r := range []struct{ release, ns string }{{"foo", a}, {"foo-canary", b}} {
		_, stderr, err := helm(t, "install", r.release, "./canary", "-n", r.ns, "--kubeconfig", kubeconfig,
			"--set", "replicaCount=1", "--set", "leaderElection.enabled=false")
		if err != nil {
			t.Errorf("helm install %s -n %s: %v: %s", r.release, r.ns, err, lastLines(stderr, 2))
		}
		release, ns := r.release, r.ns
		t.Cleanup(func() { helm(t, "uninstall", release, "-n", ns, "--kubeconfig", kubeconfig) })
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
