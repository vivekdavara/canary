package charts_test

// Review (day 5, second round): the fixes' own claims, checked value by value.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/canary/internal/analysis"
	"github.com/vivekdavara/canary/internal/testenv"
)

// The URL pattern is meant to be a subset of what url.Parse and analysis.NewPrometheus accept, but its IPv6 branch is
// "\[[0-9A-Fa-f:.]+\]": any run of hex digits, colons and dots in brackets. Go 1.27's url.Parse checks a bracketed
// host with netip.ParseAddr and refuses each of these, so the operator exits at start ("--prometheus-url: ... invalid
// host" / "invalid IP-literal"): a crash loop the schema was to prevent. (An IPv4 address in brackets is refused by
// url.Parse too.)
func TestReviewD5BSchemaAcceptsIPLiteralsTheOperatorRefuses(t *testing.T) {
	for _, u := range []string{"http://[1]", "http://[::1.2]", "http://[1.2.3.4]:9090", "http://[1:2:3:4:5:6:7:8:9]", "http://[:]"} {
		_, _, err := helm(t, "template", "canary", "./canary", "--set", "prometheus.url="+u)
		if err != nil {
			continue // refused by the schema: what the fix promises
		}
		if _, perr := analysis.NewPrometheus(u, &http.Client{}); perr != nil {
			t.Errorf("the schema accepts prometheus.url=%s, and the operator refuses it at start: %v", u, perr)
		}
	}
}

// The duration pattern is meant to accept only positive durations, and its "not" refuses a value made only of zeros
// and dots. But time.ParseDuration truncates a fraction of a nanosecond, so 0.5ns, 0.0009µs or 0s0.5ns parse as 0,
// and run refuses them ("--analysis-timeout must be positive, not 0s"): a crash loop.
func TestReviewD5BSchemaAcceptsSubNanosecondTimeouts(t *testing.T) {
	for _, v := range []string{"0.5ns", "0.999999999ns", "0.0009µs", "0.0009us", "0s0.5ns"} {
		_, _, err := helm(t, "template", "canary", "./canary", "--set", "analysisTimeout="+v)
		if err != nil {
			continue
		}
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			t.Errorf("the schema accepts analysisTimeout=%s, which parses as %v (%v): the operator refuses it at start", v, d, perr)
		}
	}
}

// A URL the schema accepts that ends in ":" (a path segment's pchar may be ":") renders the argument
// "- --prometheus-url=http://p:9090/x:" unquoted, which YAML reads as a mapping {"--prometheus-url=http://p:9090/x":
// null}, not a string: the API server refuses the Deployment. The arguments need quoting.
func TestReviewD5BAURLEndingInAColonBreaksTheArguments(t *testing.T) {
	const u = "http://p:9090/x:"
	if _, err := analysis.NewPrometheus(u, &http.Client{}); err != nil {
		t.Fatalf("the operator refuses %s itself: %v", u, err)
	}
	if _, _, err := helm(t, "template", "canary", "./canary", "--set", "prometheus.url="+u); err != nil {
		t.Skipf("the schema refuses %s", u)
	}
	dep := byKey(render(t, "canary", "--set", "prometheus.url="+u))["Deployment/canary-system/canary"]
	args := field(t, dep, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"].([]any)
	found := false
	for _, a := range args {
		if s, ok := a.(string); ok && s == "--prometheus-url="+u {
			found = true
		} else if !ok {
			t.Errorf("an argument renders as %T %v, not a string", a, a)
		}
	}
	if !found {
		t.Errorf("no --prometheus-url=%s in the arguments %v", u, args)
	}

	// And a real install is refused.
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	_, stderr, err := helm(t, "install", "colon", "./canary", "-n", ns, "--kubeconfig", kubeconfig,
		"--set", "prometheus.url="+u, "--set", "replicaCount=1", "--set", "leaderElection.enabled=false")
	t.Cleanup(func() { helm(t, "uninstall", "colon", "-n", ns, "--kubeconfig", kubeconfig) })
	if err != nil {
		t.Errorf("the schema accepted prometheus.url=%s and the install failed: %s", u, lastLines(stderr, 2))
	}
}

// The imagePullSecrets fix renders "- name: {{ . }}" unquoted (before it, toYaml quoted the {name: ...} form). A
// secret's name is a DNS subdomain, so "123", "1e3", "yes", "on" and "null" are valid names, and YAML reads each
// unquoted as a number, a boolean or null: the install fails (or the name is lost). The {name: "123"} form, which
// toYaml rendered correctly before the fix, is broken by it too.
func TestReviewD5BImagePullSecretNamesAreNotQuoted(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	for i, c := range []struct{ flag, set, name string }{
		{"--set-string", "imagePullSecrets[0]=123", "123"},
		{"--set", "imagePullSecrets[0]=yes", "yes"},
		{"--set", "imagePullSecrets[0]=1e3", "1e3"},
		{"--set-string", "imagePullSecrets[0].name=123", "123"},
		{"--set-string", "imagePullSecrets[0]=null", "null"},
	} {
		release := fmt.Sprintf("pull-%d", i)
		_, stderr, err := helm(t, "install", release, "./canary", "-n", ns, "--kubeconfig", kubeconfig,
			c.flag, c.set, "--set", "replicaCount=1", "--set", "leaderElection.enabled=false")
		t.Cleanup(func() { helm(t, "uninstall", release, "-n", ns, "--kubeconfig", kubeconfig) })
		if err != nil {
			if strings.Contains(stderr, "values don't meet the specifications") {
				continue // the schema refused it: no crash, at least
			}
			t.Errorf("%s %s: the schema accepted it and the install failed: %s", c.flag, c.set, lastLines(stderr, 2))
			continue
		}
		stdout, _, err := env.Kubectl("get", "deployment", release, "-n", ns, "-o", "jsonpath={.spec.template.spec.imagePullSecrets[0].name}")
		if err != nil || stdout != c.name {
			t.Errorf("%s %s: installed with the pull secret %q, want %q (%v)", c.flag, c.set, stdout, c.name, err)
		}
	}
}

// Turning leader election off on an installed release (helm upgrade --set replicaCount=1 --set
// leaderElection.enabled=false) is how a user would move to one replica. The fix renders strategy {type: Recreate}
// then, but the live Deployment has the defaulted rollingUpdate {maxSurge, maxUnavailable}, which nobody's manifest
// set; if the upgrade's patch keeps it, the API server refuses "rollingUpdate ... may not be specified when strategy
// type is 'Recreate'".
func TestReviewD5BUpgradeToNoLeaderElection(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	if _, stderr, err := helm(t, "install", "switch", "./canary", "-n", ns, "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("helm install: %v: %s", err, stderr)
	}
	t.Cleanup(func() { helm(t, "uninstall", "switch", "-n", ns, "--kubeconfig", kubeconfig) })
	if _, stderr, err := helm(t, "upgrade", "switch", "./canary", "-n", ns, "--kubeconfig", kubeconfig, "--reuse-values",
		"--set", "replicaCount=1", "--set", "leaderElection.enabled=false"); err != nil {
		t.Fatalf("helm upgrade to one replica without leader election: %v: %s", err, lastLines(stderr, 3))
	}
	stdout, _, err := env.Kubectl("get", "deployment", "switch", "-n", ns, "-o", "jsonpath={.spec.strategy.type}")
	if err != nil || stdout != "Recreate" {
		t.Errorf("strategy %q after the upgrade, want Recreate (%v)", stdout, err)
	}
}
