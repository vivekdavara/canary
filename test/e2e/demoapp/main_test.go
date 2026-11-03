package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := configFromEnv(env(nil))
	if err != nil || cfg != (config{ready: true}) {
		t.Fatalf("defaults: %+v, %v", cfg, err)
	}
	cfg, err = configFromEnv(env(map[string]string{"VERSION": "v2", "ERROR_RATIO": "0.25", "READY": "false"}))
	if err != nil || cfg != (config{version: "v2", errorRatio: 0.25}) {
		t.Fatalf("set: %+v, %v", cfg, err)
	}
	for _, bad := range []map[string]string{
		{"ERROR_RATIO": "-0.1"}, {"ERROR_RATIO": "1.5"}, {"ERROR_RATIO": "NaN"}, {"ERROR_RATIO": "half"},
		{"ERROR_RATIO": "Inf"}, {"READY": "maybe"},
	} {
		if _, err := configFromEnv(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// Any n consecutive requests fail floor or ceil of n * ratio of them, from the first request on.
func TestFailuresFollowTheRatio(t *testing.T) {
	for _, ratio := range []float64{0, 0.01, 0.1, 0.25, 1.0 / 3, 0.5, 0.9, 1} {
		a := newApp(config{errorRatio: ratio})
		fails := make([]bool, 1000)
		for i := range fails {
			fails[i] = a.next()
		}
		for _, n := range []int{1, 2, 3, 10, 100, 1000} {
			for start := 0; start+n <= len(fails); start += n {
				got := 0
				for _, f := range fails[start : start+n] {
					if f {
						got++
					}
				}
				want := float64(n) * ratio
				if float64(got) < want-1 || float64(got) > want+1 {
					t.Fatalf("ratio %g: requests %d..%d failed %d, want about %g", ratio, start+1, start+n, got, want)
				}
			}
		}
		if a.served != 1000 || a.failed != uint64(1000*ratio) {
			t.Errorf("ratio %g: counted %d served, %d failed", ratio, a.served, a.failed)
		}
	}
}

func TestTheHandler(t *testing.T) {
	a := newApp(config{version: "v2", errorRatio: 0.5, ready: true})
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	codes := []int{}
	for range 4 {
		codes = append(codes, status(t, srv.URL+"/"))
	}
	if want := []int{200, 500, 200, 500}; !equal(codes, want) {
		t.Errorf("codes %v, want %v", codes, want)
	}
	// Only / counts: the probes and the scrapes don't.
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if c := status(t, srv.URL+p); c != 200 {
			t.Errorf("%s: %d", p, c)
		}
	}
	if c := status(t, srv.URL+"/other"); c != 404 {
		t.Errorf("/other: %d, want 404", c)
	}
	want := `# HELP demo_requests_total Requests to /, by status code.
# TYPE demo_requests_total counter
demo_requests_total{code="200"} 2
demo_requests_total{code="500"} 2
# HELP demo_info The version this pod runs (VERSION).
# TYPE demo_info gauge
demo_info{version="v2"} 1
`
	if got := body(t, srv.URL+"/metrics"); got != want {
		t.Errorf("metrics:\n%s\nwant:\n%s", got, want)
	}
}

// A version that hasn't failed yet exports its 500s at 0, so an error-rate quotient is 0, not empty.
func TestBothCodesAreExportedFromTheStart(t *testing.T) {
	m := newApp(config{version: `a"b`}).metrics()
	for _, line := range []string{`demo_requests_total{code="200"} 0`, `demo_requests_total{code="500"} 0`, `demo_info{version="a\"b"} 1`} {
		if !strings.Contains(m, line+"\n") {
			t.Errorf("no %q in:\n%s", line, m)
		}
	}
}

func TestNotReady(t *testing.T) {
	srv := httptest.NewServer(newApp(config{}).handler())
	defer srv.Close()
	if c := status(t, srv.URL+"/readyz"); c != http.StatusServiceUnavailable {
		t.Errorf("/readyz with READY=false: %d", c)
	}
	if c := status(t, srv.URL+"/healthz"); c != 200 {
		t.Errorf("/healthz with READY=false: %d", c)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l, newApp(config{ready: true})) }()
	if c := status(t, "http://"+l.Addr().String()+"/readyz"); c != 200 {
		t.Fatalf("/readyz: %d", c)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve didn't stop")
	}
}

// load sends at its rate, one connection per request, and counts what came back.
func TestLoad(t *testing.T) {
	var conns, requests atomic.Int64
	a := newApp(config{errorRatio: 0.5, ready: true})
	srv := httptest.NewUnstartedServer(a.handler())
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		inner.ServeHTTP(w, r)
	})
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var logged atomic.Int64
	c := load(ctx, srv.URL+"/", 100, 100*time.Millisecond, func(string, ...any) { logged.Add(1) })
	// 100 a second for half a second; a loaded machine's ticker drops ticks, never adds them.
	if c.sent < 20 || c.sent > 51 {
		t.Errorf("sent %d in 0.5 s at 100/s", c.sent)
	}
	if c.ok+c.failed+c.errors != c.sent || c.errors != 0 || c.skipped != 0 {
		t.Errorf("counts don't add up: %s", c)
	}
	if d := int64(c.failed) - int64(c.ok); d < -1 || d > 1 {
		t.Errorf("a 0.5 ratio answered %d ok and %d failed", c.ok, c.failed)
	}
	if conns.Load() != requests.Load() {
		t.Errorf("%d requests on %d connections: want one each", requests.Load(), conns.Load())
	}
	if logged.Load() == 0 {
		t.Error("no progress logged")
	}
}

func TestLoadCountsNoAnswer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := load(ctx, "http://"+addr+"/", 50, time.Hour, func(string, ...any) {})
	if c.sent == 0 || c.errors != c.sent {
		t.Errorf("a refused connection: %s", c)
	}
}

func TestRunRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"other"}, {"load"}, {"load", "--url", "http://x", "--rps", "0"}, {"load", "--url", "http://x", "--rps", "NaN"}} {
		if err := run(context.Background(), args, env(nil)); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	if err := run(context.Background(), []string{"serve"}, env(map[string]string{"ERROR_RATIO": "2"})); err == nil {
		t.Error("serve with ERROR_RATIO=2 started")
	}
}

func status(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func body(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
