package analysis

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

func metric(min, max *float64) v1.Metric {
	return v1.Metric{Name: "error-rate", Query: "q", Min: min, Max: max}
}

func f(v float64) *float64 { return &v }

func vector(vs ...float64) Result { return Result{Type: "vector", Values: vs} }

// Every way a metric can fail, and the bounds being inclusive. The fail-closed cases (no samples, several, NaN, the
// infinities, a matrix, an error) are the ones a careless check would pass.
func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		m        v1.Metric
		r        Result
		err      error
		passed   bool
		measured bool
		why      string
	}{
		{"within max", metric(nil, f(0.01)), vector(0.002), nil, true, true, "max 0.01"},
		{"equal to max", metric(nil, f(0.01)), vector(0.01), nil, true, true, "max 0.01"},
		{"above max", metric(nil, f(0.01)), vector(0.0100001), nil, false, true, "above max 0.01"},
		{"equal to min", metric(f(0.5), nil), Result{Type: "scalar", Values: []float64{0.5}}, nil, true, true, "min 0.5"},
		{"below min", metric(f(0.5), nil), vector(0.49), nil, false, true, "below min 0.5"},
		{"within both", metric(f(1), f(2)), vector(1.5), nil, true, true, "min 1, max 2"},
		{"min equals max", metric(f(1), f(1)), vector(1), nil, true, true, "min 1, max 1"},
		{"negative", metric(f(-1), f(1)), vector(-0.5), nil, true, true, "min -1, max 1"},
		{"no samples", metric(nil, f(1)), vector(), nil, false, false, "the query returned no samples"},
		{"two samples", metric(nil, f(1)), vector(0.1, 0.2), nil, false, false, "the query returned 2 samples, not one"},
		{"NaN", metric(nil, f(1)), vector(math.NaN()), nil, false, true, "not a finite number"},
		{"+Inf", metric(f(0), nil), vector(math.Inf(1)), nil, false, true, "not a finite number"},
		{"-Inf", metric(nil, f(0)), vector(math.Inf(-1)), nil, false, true, "not a finite number"},
		{"a matrix", metric(nil, f(1)), Result{Type: "matrix"}, nil, false, false, "the query returned a matrix, not one number"},
		{"a string", metric(nil, f(1)), Result{Type: "string"}, nil, false, false, "the query returned a string, not one number"},
		{"an error", metric(nil, f(1)), vector(0), errors.New("bad_data: parse error"), false, false, "the query failed: bad_data: parse error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.m, tc.r, tc.err)
			if got.Passed != tc.passed || got.Measured != tc.measured || got.Why != tc.why || got.Name != "error-rate" {
				t.Fatalf("Evaluate = %+v; want passed %v, measured %v, why %q", got, tc.passed, tc.measured, tc.why)
			}
		})
	}
}

func TestMessages(t *testing.T) {
	c := Check{Metrics: []MetricResult{
		Evaluate(v1.Metric{Name: "p95", Max: f(0.3)}, vector(0.21), nil),
		Evaluate(v1.Metric{Name: "error-rate", Max: f(0.01)}, vector(0.05), nil),
		Evaluate(v1.Metric{Name: "throughput", Min: f(1)}, vector(), nil),
	}}
	want := "error-rate 0.05 (above max 0.01); throughput: the query returned no samples; p95 0.21 (max 0.3)"
	if c.Passed() || c.String() != want {
		t.Fatalf("passed %v, %q; want failed, %q", c.Passed(), c.String(), want)
	}
	if !(Check{Metrics: c.Metrics[:1]}).Passed() {
		t.Fatal("one passing metric: want passed")
	}
	// A long error is cut at a rune's start, so the message stays valid UTF-8.
	long := strings.Repeat("é", 150) // 300 bytes
	m := Evaluate(metric(nil, f(1)), Result{}, errors.New(long))
	if !strings.HasSuffix(m.Why, "...") || len(m.Why) > len("the query failed: ")+maxWhy+3 || !utf8Valid(m.Why) {
		t.Fatalf("why %q (%d bytes); want cut to %d bytes at a rune start", m.Why, len(m.Why), maxWhy)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// fakeQuerier answers each query from a map, after an optional delay that respects the context.
type fakeQuerier struct {
	answers  map[string]Result
	errs     map[string]error
	delay    time.Duration
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (q *fakeQuerier) Query(ctx context.Context, query string) (Result, error) {
	n := q.inFlight.Add(1)
	defer q.inFlight.Add(-1)
	for {
		m := q.maxSeen.Load()
		if n <= m || q.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(q.delay):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return q.answers[query], q.errs[query]
}

func TestRunChecksEveryMetricInParallel(t *testing.T) {
	q := &fakeQuerier{
		answers: map[string]Result{"a": vector(1), "b": vector(5)},
		errs:    map[string]error{"c": errors.New("server_error")},
		delay:   50 * time.Millisecond,
	}
	metrics := []v1.Metric{{Name: "a", Query: "a", Max: f(2)}, {Name: "b", Query: "b", Max: f(2)}, {Name: "c", Query: "c", Max: f(2)}}
	start := time.Now()
	c := Run(context.Background(), q, metrics, time.Second)
	if took := time.Since(start); took > 140*time.Millisecond {
		t.Errorf("three 50ms queries took %v; want them in parallel", took)
	}
	if q.maxSeen.Load() != 3 {
		t.Errorf("at most %d queries at once; want 3", q.maxSeen.Load())
	}
	// In the metrics' order, whatever order the queries finished in.
	got := []bool{c.Metrics[0].Passed, c.Metrics[1].Passed, c.Metrics[2].Passed}
	if c.Metrics[0].Name != "a" || c.Metrics[2].Name != "c" || got[0] != true || got[1] != false || got[2] != false || c.Passed() {
		t.Fatalf("check %+v", c)
	}
}

func TestRunTimesOutEachQuery(t *testing.T) {
	q := &fakeQuerier{answers: map[string]Result{"slow": vector(0)}, delay: time.Hour}
	start := time.Now()
	c := Run(context.Background(), q, []v1.Metric{{Name: "slow", Query: "slow", Max: f(1)}}, 30*time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v; want the 30ms timeout", took)
	}
	if c.Passed() || !strings.Contains(c.Metrics[0].Why, "deadline exceeded") {
		t.Fatalf("check %+v; want failed on the deadline", c)
	}
}

func TestRunWithoutPrometheusFailsClosed(t *testing.T) {
	c := Run(context.Background(), nil, []v1.Metric{{Name: "a", Query: "a", Max: f(1)}, {Name: "b", Query: "b", Min: f(0)}}, time.Second)
	if c.Passed() || len(c.Metrics) != 2 || !strings.Contains(c.Metrics[1].Why, "--prometheus-url") {
		t.Fatalf("check %+v; want both failed for want of Prometheus", c)
	}
}
