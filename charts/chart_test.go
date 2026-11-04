// Package charts_test checks the Helm chart (charts/canary) with the pinned helm (make test sets CANARY_HELM): what
// it renders, that it installs the same objects as config/default, what it refuses, and a real install, upgrade and
// uninstall against envtest's API server (with no CRD installed beforehand, so the chart's own is the one served).
package charts_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.MainWithoutCRDs(m)) }

// helm runs the pinned helm from this directory and returns what it wrote. Without CANARY_HELM the test skips, or
// fails where a check must not pass by skipping (CANARY_REQUIRE_ENVTEST, set by make test and CI).
func helm(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	bin := os.Getenv("CANARY_HELM")
	if bin == "" {
		if os.Getenv("CANARY_REQUIRE_ENVTEST") != "" {
			t.Fatal("CANARY_REQUIRE_ENVTEST is set but CANARY_HELM isn't (make test sets both)")
		}
		t.Skip("no helm: set CANARY_HELM (make test does)")
	}
	var o, e bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// render is helm template for a release in canary-system, decoded.
func render(t *testing.T, release string, args ...string) []*unstructured.Unstructured {
	t.Helper()
	stdout, stderr, err := helm(t, append([]string{"template", release, "./canary", "-n", "canary-system"}, args...)...)
	if err != nil {
		t.Fatalf("helm template %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return decode(t, stdout)
}

func decode(t *testing.T, manifests string) []*unstructured.Unstructured {
	t.Helper()
	var objs []*unstructured.Unstructured
	d := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifests), 4096)
	for {
		var obj map[string]any
		if err := d.Decode(&obj); errors.Is(err, io.EOF) {
			return objs
		} else if err != nil {
			t.Fatalf("decoding the manifests: %v", err)
		}
		if obj != nil {
			objs = append(objs, &unstructured.Unstructured{Object: obj})
		}
	}
}

// key names an object the way kubectl does: kind/namespace/name.
func key(o *unstructured.Unstructured) string {
	return o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
}

func byKey(objs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	m := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		m[key(o)] = o
	}
	return m
}

func field(t *testing.T, o *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(o.Object, path...)
	if err != nil || !ok {
		t.Fatalf("%s has no %s (%v)", key(o), strings.Join(path, "."), err)
	}
	return v
}

// A release named canary in canary-system installs what config/default does, object for object, with the same names:
// the same CRD, the same rules granted to the same account, and the same pod spec for the operator. The differences
// are the chart's own: helm install --create-namespace makes the namespace, the objects carry Helm's labels (the
// selector the release's too), and the operator gets --analysis-timeout spelled out (10s, the binary's default).
func TestTheChartInstallsWhatConfigDefaultDoes(t *testing.T) {
	env := testenv.Get(t)
	stdout, stderr, err := env.Kubectl("kustomize", "../config/default")
	if err != nil {
		t.Fatalf("kubectl kustomize: %v: %s", err, stderr)
	}
	kustomized := byKey(decode(t, stdout))
	chart := byKey(render(t, "canary", "--include-crds"))

	delete(kustomized, "Namespace//canary-system")
	var missing, extra []string
	for k := range kustomized {
		if chart[k] == nil {
			missing = append(missing, k)
		}
	}
	for k := range chart {
		if kustomized[k] == nil {
			extra = append(extra, k)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("the chart's objects differ from config/default's: missing %v, extra %v", missing, extra)
	}

	same := func(k string, path ...string) {
		t.Helper()
		if a, b := field(t, kustomized[k], path...), field(t, chart[k], path...); !reflect.DeepEqual(a, b) {
			t.Errorf("%s %s differs:\nconfig/default: %v\nchart:          %v", k, strings.Join(path, "."), a, b)
		}
	}
	same("CustomResourceDefinition//canaryrollouts.rollouts.vivekdavara.github.io", "spec")
	same("ClusterRole//canary-manager", "rules")
	same("Role/canary-system/canary-leader-election", "rules")
	for _, k := range []string{"ClusterRoleBinding//canary-manager", "RoleBinding/canary-system/canary-leader-election"} {
		same(k, "roleRef")
		same(k, "subjects")
	}
	same("Deployment/canary-system/canary", "spec", "replicas")

	// The pod spec, with the chart's one extra argument taken out.
	podSpec := func(o *unstructured.Unstructured) map[string]any {
		spec := field(t, o.DeepCopy(), "spec", "template", "spec").(map[string]any)
		c := spec["containers"].([]any)[0].(map[string]any)
		var args []any
		for _, a := range c["args"].([]any) {
			if a != "--analysis-timeout=10s" {
				args = append(args, a)
			}
		}
		c["args"] = args
		return spec
	}
	dep := "Deployment/canary-system/canary"
	if a, b := podSpec(kustomized[dep]), podSpec(chart[dep]); !reflect.DeepEqual(a, b) {
		t.Errorf("the operator's pod spec differs:\nconfig/default: %v\nchart:          %v", a, b)
	}
	if got := field(t, chart[dep], "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"]; !reflect.DeepEqual(got,
		[]any{"--leader-elect", "--metrics-bind-address=:8111", "--health-probe-bind-address=:8121", "--analysis-timeout=10s"}) {
		t.Errorf("the chart's default arguments: %v", got)
	}
}

// The chart's copies of the generated files are the generated files (make manifests copies them; CI fails if a copy
// wasn't committed). Byte for byte, so the CRD's descriptions are the same too.
func TestTheChartsCopiesAreTheGeneratedFiles(t *testing.T) {
	for copied, generated := range map[string]string{
		"canary/crds/rollouts.vivekdavara.github.io_canaryrollouts.yaml": "../config/crd/bases/rollouts.vivekdavara.github.io_canaryrollouts.yaml",
		"canary/files/role.yaml": "../config/rbac/role.yaml",
	} {
		a, err := os.ReadFile(copied)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s isn't a copy of %s: run make manifests", copied, generated)
		}
	}
}

// Objects are named after the release (a release that doesn't say canary gets "-canary" appended), and one replica
// without leader election renders neither the flag nor the Lease's Role.
func TestTheChartsNamesAndLeaderElection(t *testing.T) {
	for release, want := range map[string]string{"canary": "canary", "canary-prod": "canary-prod", "rollouts": "rollouts-canary"} {
		objs := byKey(render(t, release))
		for _, k := range []string{
			"ServiceAccount/canary-system/" + want, "ClusterRole//" + want + "-manager", "ClusterRoleBinding//" + want + "-manager",
			"Role/canary-system/" + want + "-leader-election", "RoleBinding/canary-system/" + want + "-leader-election",
			"Deployment/canary-system/" + want,
		} {
			if objs[k] == nil {
				t.Errorf("release %s: no %s", release, k)
			}
		}
		if len(objs) != 6 {
			t.Errorf("release %s: %d objects, want 6", release, len(objs))
		}
		if sa := field(t, objs["Deployment/canary-system/"+want], "spec", "template", "spec", "serviceAccountName"); sa != want {
			t.Errorf("release %s runs as %v, want %s", release, sa, want)
		}
	}

	objs := byKey(render(t, "canary", "--set", "replicaCount=1", "--set", "leaderElection.enabled=false",
		"--set", "prometheus.url=http://prometheus.monitoring.svc:9090/prom", "--set", "analysisTimeout=1m30s"))
	if objs["Role/canary-system/canary-leader-election"] != nil || objs["RoleBinding/canary-system/canary-leader-election"] != nil {
		t.Error("the Lease's Role is rendered without leader election")
	}
	dep := objs["Deployment/canary-system/canary"]
	args := field(t, dep, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"]
	if want := []any{"--metrics-bind-address=:8111", "--health-probe-bind-address=:8121",
		"--prometheus-url=http://prometheus.monitoring.svc:9090/prom", "--analysis-timeout=1m30s"}; !reflect.DeepEqual(args, want) {
		t.Errorf("arguments %v, want %v", args, want)
	}
	if r := field(t, dep, "spec", "replicas"); fmt.Sprint(r) != "1" {
		t.Errorf("replicas %v", r)
	}
}

// Values that would make an operator that can't work are refused at install, each with a message saying which.
func TestTheChartRefusesBadValues(t *testing.T) {
	for _, c := range []struct {
		set  []string
		want string
	}{
		{[]string{"leaderElection.enabled=false"}, "replicaCount above 1 needs leaderElection.enabled"},
		{[]string{"replicaCount=0"}, "at '/replicaCount': minimum: got 0, want 1"},
		{[]string{"prometheus.url=http://p:9090?"}, "at '/prometheus/url'"},
		{[]string{"prometheus.url=http://p:9090#x"}, "at '/prometheus/url'"},
		{[]string{"prometheus.url=prometheus:9090"}, "at '/prometheus/url'"},
		{[]string{"prometheus.url=http://user@p:9090"}, "at '/prometheus/url'"},
		{[]string{"analysisTimeout=10"}, "at '/analysisTimeout'"},
		{[]string{"analysisTimeout=ten seconds"}, "at '/analysisTimeout'"},
		{[]string{"image.repository="}, "at '/image/repository'"},
		{[]string{"metricsPort=0"}, "at '/metricsPort'"},
		{[]string{"prometheusUrl=http://p:9090"}, "additional properties 'prometheusUrl' not allowed"},
	} {
		args := []string{"template", "canary", "./canary"}
		for _, s := range c.set {
			args = append(args, "--set", s)
		}
		_, stderr, err := helm(t, args...)
		if err == nil {
			t.Errorf("--set %v rendered", c.set)
		} else if !strings.Contains(stderr, c.want) {
			t.Errorf("--set %v failed without %q: %s", c.set, c.want, stderr)
		}
	}
}

// Without prometheus.url the install says what that means (every check fails closed); with it, it doesn't.
func TestTheChartsNotesWarnWithoutPrometheus(t *testing.T) {
	stdout, stderr, err := helm(t, "install", "canary", "./canary", "-n", "canary-system", "--dry-run=client")
	if err != nil {
		t.Fatalf("helm install --dry-run=client: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "No prometheus.url is set: every analysis check fails closed") {
		t.Errorf("no warning in the notes:\n%s", stdout)
	}
	stdout, stderr, err = helm(t, "install", "canary", "./canary", "-n", "canary-system", "--dry-run=client",
		"--set", "prometheus.url=http://p:9090")
	if err != nil {
		t.Fatalf("helm install --dry-run=client: %v: %s", err, stderr)
	}
	if strings.Contains(stdout, "No prometheus.url") {
		t.Errorf("a warning with the URL set:\n%s", stdout)
	}
}

// helm install, upgrade and uninstall against a real API server. The install creates the namespace and the CRD (this
// package's API server starts without it), and the operator's service account may do what config/default's may and
// no more. An upgrade changes the operator's arguments and keeps the rest. An uninstall removes the operator and
// keeps the CRD and the rollouts in it (Helm never deletes a chart's crds/), so uninstalling the operator doesn't
// delete anyone's rollouts.
func TestHelmInstallUpgradeUninstall(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	stdout, stderr, err := helm(t, "install", "canary", "./canary", "-n", "canary-system", "--create-namespace",
		"--kubeconfig", kubeconfig, "--set", "prometheus.url=http://prometheus.monitoring.svc:9090")
	if err != nil {
		t.Fatalf("helm install: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "STATUS: deployed") {
		t.Errorf("helm install said:\n%s", stdout)
	}
	if _, stderr, err := env.Kubectl("wait", "--for=condition=Established", "--timeout=30s",
		"crd/canaryrollouts.rollouts.vivekdavara.github.io"); err != nil {
		t.Fatalf("the chart's CRD isn't served: %v: %s", err, stderr)
	}
	env.CheckOperatorPermissions(t, "canary-system:canary", "canary-system")

	args := func() string {
		t.Helper()
		stdout, stderr, err := env.Kubectl("get", "deployment", "canary", "-n", "canary-system", "-o",
			"jsonpath={.spec.template.spec.containers[0].args}")
		if err != nil {
			t.Fatalf("kubectl get deployment: %v: %s", err, stderr)
		}
		return stdout
	}
	if got, want := args(), `["--leader-elect","--metrics-bind-address=:8111","--health-probe-bind-address=:8121","--prometheus-url=http://prometheus.monitoring.svc:9090","--analysis-timeout=10s"]`; got != want {
		t.Errorf("installed with %s\nwant %s", got, want)
	}

	ns := env.Namespace(t)
	if _, stderr, err := env.Kubectl("apply", "-n", ns, "-f", "../config/samples/web.yaml"); err != nil {
		t.Fatalf("the sample, on the chart's CRD: %v: %s", err, stderr)
	}

	if _, stderr, err := helm(t, "upgrade", "canary", "./canary", "-n", "canary-system", "--kubeconfig", kubeconfig,
		"--reuse-values", "--set", "analysisTimeout=30s"); err != nil {
		t.Fatalf("helm upgrade: %v: %s", err, stderr)
	}
	if got, want := args(), `["--leader-elect","--metrics-bind-address=:8111","--health-probe-bind-address=:8121","--prometheus-url=http://prometheus.monitoring.svc:9090","--analysis-timeout=30s"]`; got != want {
		t.Errorf("upgraded to %s\nwant %s", got, want)
	}

	if _, stderr, err := helm(t, "uninstall", "canary", "-n", "canary-system", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("helm uninstall: %v: %s", err, stderr)
	}
	for _, obj := range []string{"deployment/canary", "serviceaccount/canary", "role/canary-leader-election", "rolebinding/canary-leader-election"} {
		if stdout, _, err := env.Kubectl("get", "-n", "canary-system", obj, "--ignore-not-found", "-o", "name"); err != nil || stdout != "" {
			t.Errorf("%s after the uninstall: %q (%v)", obj, stdout, err)
		}
	}
	for _, obj := range []string{"clusterrole/canary-manager", "clusterrolebinding/canary-manager"} {
		if stdout, _, err := env.Kubectl("get", obj, "--ignore-not-found", "-o", "name"); err != nil || stdout != "" {
			t.Errorf("%s after the uninstall: %q (%v)", obj, stdout, err)
		}
	}
	if stdout, stderr, err := env.Kubectl("get", "crl", "web", "-n", ns, "-o", "name"); err != nil ||
		strings.TrimSpace(stdout) != "canaryrollout.rollouts.vivekdavara.github.io/web" {
		t.Errorf("the rollout after the uninstall: %q (%v: %s)", stdout, err, stderr)
	}
}
