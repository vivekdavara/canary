// Command demoapp is the application the end-to-end tests roll out (test/e2e, DESIGN.md "End to end"). It serves
// HTTP and fails a configured share of its requests, counting them by status code in Prometheus's text format, so a
// version with a higher error ratio fails a real analysis against a real Prometheus.
//
//	demoapp serve [--listen :8080]          with VERSION, ERROR_RATIO (0 to 1) and READY (true/false) from the environment
//	demoapp load --url URL [--rps 20]       sends GET URL rps times a second, a new connection each
//
// The load mode opens a new connection per request so that kube-proxy, which balances connections, spreads the
// requests over the Service's pods in proportion to their count: the replica-ratio split the operator relies on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "demoapp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	if len(args) == 0 {
		return errors.New("usage: demoapp serve|load [flags]")
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		listen := fs.String("listen", ":8080", "where to serve the application, /metrics, /healthz and /readyz")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := configFromEnv(getenv)
		if err != nil {
			return err
		}
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		log.Printf("serving on %s: version %q, error ratio %g, ready %t", l.Addr(), cfg.version, cfg.errorRatio, cfg.ready)
		return serve(ctx, l, newApp(cfg))
	case "load":
		fs := flag.NewFlagSet("load", flag.ContinueOnError)
		target := fs.String("url", "", "the URL to send GET requests to (required)")
		rps := fs.Float64("rps", 20, "requests a second")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *target == "" {
			return errors.New("load: --url is required")
		}
		if !(*rps > 0 && *rps <= 1000) {
			return fmt.Errorf("load: --rps must be in (0, 1000], not %g", *rps)
		}
		counts := load(ctx, *target, *rps, 10*time.Second, log.Printf)
		log.Printf("stopped: %s", counts)
		return nil
	default:
		return fmt.Errorf("unknown mode %q: serve or load", args[0])
	}
}

type config struct {
	version    string
	errorRatio float64
	ready      bool
}

// configFromEnv reads VERSION, ERROR_RATIO (default 0) and READY (default true). A bad value is an error, not a
// default: a typo in a test's template should fail the pod, not silently run a healthy version.
func configFromEnv(getenv func(string) string) (config, error) {
	cfg := config{version: getenv("VERSION"), ready: true}
	if s := getenv("ERROR_RATIO"); s != "" {
		r, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(r) || r < 0 || r > 1 {
			return config{}, fmt.Errorf("ERROR_RATIO must be a number from 0 to 1, not %q", s)
		}
		cfg.errorRatio = r
	}
	if s := getenv("READY"); s != "" {
		r, err := strconv.ParseBool(s)
		if err != nil {
			return config{}, fmt.Errorf("READY must be true or false, not %q", s)
		}
		cfg.ready = r
	}
	return cfg, nil
}

// app is the served application. Its failures are deterministic: request n fails when floor(n * ratio) goes up, so
// any n consecutive requests fail floor or ceil of n * ratio of them (a ratio of 0.5 fails every second one).
type app struct {
	cfg config

	mu     sync.Mutex
	served uint64
	failed uint64
}

func newApp(cfg config) *app { return &app{cfg: cfg} }

// next counts a request and says whether it fails.
func (a *app) next() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.served + 1
	a.served = n
	fail := math.Floor(float64(n)*a.cfg.errorRatio) > math.Floor(float64(n-1)*a.cfg.errorRatio)
	if fail {
		a.failed++
	}
	return fail
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		if a.next() {
			http.Error(w, "failed on purpose (ERROR_RATIO)", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "demoapp %s\n", a.cfg.version)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, a.metrics())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.cfg.ready {
			http.Error(w, "not ready (READY=false)", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	return mux
}

// metrics is the exposition. Both codes are there from the start, at 0: an error-rate query divides the 500s'
// rate by all requests', and a series that doesn't exist yet would make the quotient empty, a failed check, for a
// version that has simply not failed.
func (a *app) metrics() string {
	a.mu.Lock()
	ok, failed := a.served-a.failed, a.failed
	a.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP demo_requests_total Requests to /, by status code.\n")
	b.WriteString("# TYPE demo_requests_total counter\n")
	fmt.Fprintf(&b, "demo_requests_total{code=\"200\"} %d\n", ok)
	fmt.Fprintf(&b, "demo_requests_total{code=\"500\"} %d\n", failed)
	b.WriteString("# HELP demo_info The version this pod runs (VERSION).\n")
	b.WriteString("# TYPE demo_info gauge\n")
	fmt.Fprintf(&b, "demo_info{version=%s} 1\n", strconv.Quote(a.cfg.version))
	return b.String()
}

// serve serves until ctx is done, then gives requests in flight 5 seconds.
func serve(ctx context.Context, l net.Listener, a *app) error {
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadCounts is what a load run saw.
type loadCounts struct {
	sent, ok, failed, errors, skipped uint64
}

func (c loadCounts) String() string {
	return fmt.Sprintf("%d sent: %d ok, %d HTTP errors, %d not answered; %d skipped (too many in flight)",
		c.sent, c.ok, c.failed, c.errors, c.skipped)
}

// maxInFlight bounds the requests waiting for an answer, so a target that hangs can't pile up goroutines.
const maxInFlight = 64

// load sends GET target rps times a second until ctx is done, each on a new connection, logging the counts every
// report interval; it returns them once the requests in flight are done.
func load(ctx context.Context, target string, rps float64, report time.Duration, logf func(string, ...any)) loadCounts {
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	var (
		mu     sync.Mutex
		counts loadCounts
		wg     sync.WaitGroup
	)
	slots := make(chan struct{}, maxInFlight)
	tick := time.NewTicker(time.Duration(float64(time.Second) / rps))
	defer tick.Stop()
	logTick := time.NewTicker(report)
	defer logTick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return counts
		case <-logTick.C:
			mu.Lock()
			logf("%s", counts)
			mu.Unlock()
		case <-tick.C:
			select {
			case slots <- struct{}{}:
			default:
				mu.Lock()
				counts.skipped++
				mu.Unlock()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				code := get(client, target)
				mu.Lock()
				defer mu.Unlock()
				counts.sent++
				switch {
				case code == 0:
					counts.errors++
				case code < 400:
					counts.ok++
				default:
					counts.failed++
				}
			}()
		}
	}
}

// get sends one request and returns its status code, or 0 with no answer.
func get(client *http.Client, target string) int {
	resp, err := client.Get(target)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
