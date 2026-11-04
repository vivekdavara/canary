package charts_test

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/canary/internal/analysis"
)

// The schema's patterns, compiled with Go's regexp, which is the engine Helm's JSON schema validator uses (a few
// accepted values go through helm too, to check that).
func schemaPatterns(t *testing.T) (url *regexp.Regexp, timeout []*regexp.Regexp) {
	t.Helper()
	data, err := os.ReadFile("canary/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Prometheus struct {
				Properties struct {
					URL struct {
						Pattern string `json:"pattern"`
					} `json:"url"`
				} `json:"properties"`
			} `json:"prometheus"`
			AnalysisTimeout struct {
				AllOf []struct {
					Pattern string `json:"pattern"`
				} `json:"allOf"`
			} `json:"analysisTimeout"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	url = regexp.MustCompile(schema.Properties.Prometheus.Properties.URL.Pattern)
	for _, p := range schema.Properties.AnalysisTimeout.AllOf {
		timeout = append(timeout, regexp.MustCompile(p.Pattern))
	}
	if len(timeout) != 2 {
		t.Fatalf("%d timeout patterns", len(timeout))
	}
	return url, timeout
}

func pick(r *rand.Rand, xs ...string) string { return xs[r.IntN(len(xs))] }

func randomFrom(r *rand.Rand, alphabet string, max int) string {
	runes := []rune(alphabet)
	n := r.IntN(max + 1)
	var b strings.Builder
	for range n {
		b.WriteRune(runes[r.IntN(len(runes))])
	}
	return b.String()
}

// Every Prometheus URL the schema accepts is one the operator accepts (analysis.NewPrometheus is what its
// --prometheus-url check calls), over 200,000 random URLs built from plausible and broken parts: hosts, bracketed
// addresses, ports, paths, escapes, users, queries. The review found five the first two patterns let through.
func TestEveryURLTheSchemaAcceptsTheOperatorAccepts(t *testing.T) {
	url, _ := schemaPatterns(t)
	r := rand.New(rand.NewPCG(1, 2))
	accepted, ipv6 := 0, 0
	var sample []string
	for range 200_000 {
		var host string
		switch r.IntN(3) {
		case 0:
			host = randomFrom(r, "abz09-._", 12)
		case 1:
			host = "[" + randomFrom(r, "0123456789abcdefABCDEF:.", 24) + "]"
		default:
			host = pick(r, "prometheus", "prometheus.monitoring.svc", "10.0.0.1", "[::1]", "[fd00::1]", "a", "p-1", "user@p", "p q", "[fd00::1")
		}
		u := pick(r, "http://", "https://", "ftp://", "http:/", "") + host
		if r.IntN(2) == 0 {
			u += ":" + randomFrom(r, "0123456789O", 6)
		}
		if r.IntN(2) == 0 {
			u += "/" + randomFrom(r, "abc/%2Fz:@?#~ -", 10)
		}
		// "" is allowed and means no Prometheus: the chart then renders no --prometheus-url.
		if u == "" || !url.MatchString(u) {
			continue
		}
		accepted++
		if strings.Contains(u, "[") {
			ipv6++
		}
		if len(sample) < 20 && r.IntN(50) == 0 {
			sample = append(sample, u)
		}
		if _, err := analysis.NewPrometheus(u, http.DefaultClient); err != nil {
			t.Fatalf("the schema accepts %q, the operator refuses it: %v", u, err)
		}
	}
	t.Logf("%d of 200,000 URLs accepted, %d of them with a bracketed address", accepted, ipv6)
	if accepted < 5_000 || ipv6 < 500 {
		t.Fatalf("the generator reached too few accepted URLs (%d, %d bracketed) to say anything", accepted, ipv6)
	}
	for _, u := range sample {
		if _, stderr, err := helm(t, "template", "canary", "./canary", "--set", "prometheus.url="+u); err != nil {
			t.Errorf("Go's regexp accepts %q, helm refuses it: %s", u, lastLines(stderr, 2))
		}
	}
}

// Every timeout the schema accepts is one the operator accepts: time.ParseDuration parses it (as the flag does) to
// more than zero (the operator's own check), over 200,000 random durations of up to nine parts, with numbers of up to
// seven digits and fractions of up to eleven, in Go's units and some that aren't. The review found the zero,
// overflowing and sub-nanosecond ones the first patterns let through.
func TestEveryTimeoutTheSchemaAcceptsTheOperatorAccepts(t *testing.T) {
	_, patterns := schemaPatterns(t)
	r := rand.New(rand.NewPCG(3, 4))
	accepted, tiny := 0, 0
	var sample []string
	for range 200_000 {
		var b strings.Builder
		for range 1 + r.IntN(9) {
			b.WriteString(randomFrom(r, "0000123456789", 7))
			if r.IntN(3) == 0 {
				b.WriteString("." + randomFrom(r, "000000123456789", 11))
			}
			b.WriteString(pick(r, "ns", "us", "µs", "μs", "ms", "s", "m", "h", "d", ""))
		}
		v := b.String()
		if !patterns[0].MatchString(v) || !patterns[1].MatchString(v) {
			continue
		}
		accepted++
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("the schema accepts %q, the operator refuses it: %v, %v", v, d, err)
		}
		if d < time.Second {
			tiny++
		}
		if len(sample) < 20 && r.IntN(20) == 0 {
			sample = append(sample, v)
		}
	}
	t.Logf("%d of 200,000 timeouts accepted, %d of them under a second", accepted, tiny)
	if accepted < 2_000 || tiny < 50 {
		t.Fatalf("the generator reached too few accepted timeouts (%d, %d under a second) to say anything", accepted, tiny)
	}
	for _, v := range sample {
		if _, stderr, err := helm(t, "template", "canary", "./canary", "--set", "analysisTimeout="+v); err != nil {
			t.Errorf("Go's regexp accepts %q, helm refuses it: %s", v, lastLines(stderr, 2))
		}
	}
	// And the boundaries, through helm.
	for v, want := range map[string]bool{
		"1ms": true, "0.5s": true, "0.000000001s": true, "10s": true, "99999h": true,
		"0s": false, "0.5ns": false, "999ns": false, "500us": false, "0.0000000001s": false, "100000h": false,
		strings.Repeat("99999h", 8): false,
	} {
		_, _, err := helm(t, "template", "canary", "./canary", "--set", "analysisTimeout="+v)
		if (err == nil) != want {
			t.Errorf("analysisTimeout=%s: rendered %v, want %v", v, err == nil, want)
		}
	}
}
