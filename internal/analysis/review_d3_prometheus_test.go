package analysis

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// Review of day 3. NewPrometheus refuses a URL with a query ("want no query"), but a URL ending in a bare "?"
// (url.URL.ForceQuery, RawQuery empty) passes the check, and the endpoint is built from u.String(), which keeps the
// "?": "http://p:9090?/api/v1/query". Every query then goes to "/" with the query string "/api/v1/query", and fails
// (Prometheus serves its UI there), so every rollout with analysis rolls back. Refusing the URL, or building the
// right endpoint, would both pass this test.
func TestReviewD3AURLEndingInAQuestionMark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.Error(w, "<html>the Prometheus UI</html>", http.StatusOK)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"scalar","result":[1,"0.001"]}}`)
	}))
	t.Cleanup(srv.Close)
	for _, base := range []string{srv.URL + "?", srv.URL + "/?"} {
		p, err := NewPrometheus(base, srv.Client())
		if err != nil {
			continue // refused at startup: fixed
		}
		if _, err := p.Query(context.Background(), "up"); err != nil {
			t.Errorf("NewPrometheus(%q) accepted it (endpoint %q), but every query fails: %v", base, p.endpoint, err)
		}
	}
}

// Review of day 3 (arguable; DESIGN.md's list of failures doesn't name it): Prometheus (and Thanos, Cortex, Mimir)
// answer a query that could only be partly evaluated (a store or remote-read endpoint down, a sample limit) with
// status "success", the partial result, and "warnings". The client ignores warnings, so a partial answer, which can
// leave out exactly the failing series, passes the check.
func TestReviewD3APartialAnswerWithWarningsDoesNotPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","warnings":["remote read: store 10.0.0.7:10901 unavailable, partial response"],`+
			`"data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.001"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	max := 0.01
	m := v1.Metric{Name: "error-rate", Query: "q", Max: &max}
	r, err := p.Query(context.Background(), m.Query)
	if res := Evaluate(m, r, err); res.Passed {
		t.Errorf("a partial answer (with warnings) passed: %s", res)
	}
}
