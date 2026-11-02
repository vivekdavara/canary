// Package analysis checks a step's metrics (DESIGN.md, "Analysis"). Each metric's query runs as a Prometheus instant
// query, and a metric passes only when the answer is one finite number within its bounds. Anything else fails it: an
// empty answer, more than one sample, NaN or an infinity, a query error or a timeout. An analysis that can't measure
// doesn't pass (it fails closed).
package analysis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// Querier runs PromQL instant queries. Prometheus (prometheus.go) is the real one; tests use fakes.
type Querier interface {
	Query(ctx context.Context, query string) (Result, error)
}

// Result is an instant query's answer. A scalar has one value, a vector one per sample; for the other result types
// (matrix, string) Values is empty and only Type says what came back.
type Result struct {
	Type   string
	Values []float64
}

// MetricResult is one metric's verdict in a check.
type MetricResult struct {
	Name string
	// Value is the number the query gave; Measured says there was exactly one.
	Value    float64
	Measured bool
	Passed   bool
	// Why says why the metric failed, or which bounds it passed.
	Why string
}

// String is "error-rate 0.002 (max 0.01)" or "error-rate: no samples".
func (m MetricResult) String() string {
	if m.Measured {
		return fmt.Sprintf("%s %s (%s)", m.Name, format(m.Value), m.Why)
	}
	return fmt.Sprintf("%s: %s", m.Name, m.Why)
}

// Check is one check of all a rollout's metrics.
type Check struct {
	Metrics []MetricResult
}

// Passed says every metric passed.
func (c Check) Passed() bool {
	for _, m := range c.Metrics {
		if !m.Passed {
			return false
		}
	}
	return true
}

// String lists the metrics, failed ones first, so a cut message still says what failed.
func (c Check) String() string {
	var failed, passed []string
	for _, m := range c.Metrics {
		if m.Passed {
			passed = append(passed, m.String())
		} else {
			failed = append(failed, m.String())
		}
	}
	return strings.Join(append(failed, passed...), "; ")
}

// Evaluate is the verdict on one metric from its query's result or error. The bounds are inclusive: a value equal to
// min or max passes.
func Evaluate(m v1.Metric, r Result, err error) MetricResult {
	res := MetricResult{Name: m.Name}
	switch {
	case err != nil:
		res.Why = "the query failed: " + shorten(err.Error())
		return res
	case r.Type != "scalar" && r.Type != "vector":
		res.Why = fmt.Sprintf("the query returned a %s, not one number", r.Type)
		return res
	case len(r.Values) == 0:
		res.Why = "the query returned no samples"
		return res
	case len(r.Values) > 1:
		res.Why = fmt.Sprintf("the query returned %d samples, not one", len(r.Values))
		return res
	}
	v := r.Values[0]
	res.Value, res.Measured = v, true
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		res.Why = "not a finite number"
	case m.Min != nil && v < *m.Min:
		res.Why = "below min " + format(*m.Min)
	case m.Max != nil && v > *m.Max:
		res.Why = "above max " + format(*m.Max)
	default:
		res.Passed = true
		res.Why = bounds(m)
	}
	return res
}

// ErrNoPrometheus fails every metric of a rollout with analysis when the operator was started without
// --prometheus-url.
var ErrNoPrometheus = errors.New("the operator has no Prometheus to ask (start it with --prometheus-url)")

// Run checks every metric once, querying them in parallel, each query bounded by timeout. A nil Querier fails them
// all with ErrNoPrometheus.
func Run(ctx context.Context, q Querier, metrics []v1.Metric, timeout time.Duration) Check {
	c := Check{Metrics: make([]MetricResult, len(metrics))}
	var wg sync.WaitGroup
	for i, m := range metrics {
		if q == nil {
			c.Metrics[i] = Evaluate(m, Result{}, ErrNoPrometheus)
			continue
		}
		wg.Go(func() {
			qctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			r, err := q.Query(qctx, m.Query)
			c.Metrics[i] = Evaluate(m, r, err)
		})
	}
	wg.Wait()
	return c
}

// bounds is "min 0.5, max 2", "max 0.01" or "min 1".
func bounds(m v1.Metric) string {
	var b []string
	if m.Min != nil {
		b = append(b, "min "+format(*m.Min))
	}
	if m.Max != nil {
		b = append(b, "max "+format(*m.Max))
	}
	return strings.Join(b, ", ")
}

// format writes a float as Prometheus does: the shortest form that reads back the same, NaN, +Inf, -Inf.
func format(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// maxWhy bounds an error's text in a metric's verdict, which ends up in a condition message and an Event.
const maxWhy = 200

// shorten cuts s to maxWhy bytes, at a rune's start.
func shorten(s string) string {
	if len(s) <= maxWhy {
		return s
	}
	i := maxWhy
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "..."
}
