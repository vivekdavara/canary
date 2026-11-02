package analysis

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// prometheus serves canned API responses (bodies in the format of Prometheus's HTTP API docs) and records the
// requests it got.
func prometheus(t *testing.T, status int, body string) (*Prometheus, *[]*http.Request, *[]string) {
	t.Helper()
	var reqs []*http.Request
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		reqs = append(reqs, r)
		queries = append(queries, r.PostForm.Get("query"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p, err := NewPrometheus(srv.URL+"/prom/", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return p, &reqs, &queries
}

func TestPrometheusAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Result
	}{
		{"scalar", `{"status":"success","data":{"resultType":"scalar","result":[1730539200.123,"0.25"]}}`,
			Result{Type: "scalar", Values: []float64{0.25}}},
		{"one sample", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"web"},"value":[1730539200,"0.002"]}]}}`,
			Result{Type: "vector", Values: []float64{0.002}}},
		{"two samples", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":"a"},"value":[1,"1"]},{"metric":{"pod":"b"},"value":[1,"2"]}]}}`,
			Result{Type: "vector", Values: []float64{1, 2}}},
		{"empty", `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			Result{Type: "vector"}},
		{"a matrix", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"1"],[2,"2"]]}]}}`,
			Result{Type: "matrix"}},
		{"with warnings", `{"status":"success","warnings":["PromQL info: metric might not be a counter"],"data":{"resultType":"vector","result":[{"metric":{},"value":[1,"3e-05"]}]}}`,
			Result{Type: "vector", Values: []float64{3e-05}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, reqs, queries := prometheus(t, http.StatusOK, tc.body)
			q := `sum(rate(http_requests_total{track="canary",code=~"5.."}[1m]))`
			got, err := p.Query(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != tc.want.Type || len(got.Values) != len(tc.want.Values) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
			for i := range got.Values {
				if got.Values[i] != tc.want.Values[i] {
					t.Fatalf("got %+v; want %+v", got, tc.want)
				}
			}
			// A POST of the query as a form, to the API under the URL's path, so long queries fit.
			r := (*reqs)[0]
			if r.Method != http.MethodPost || r.URL.Path != "/prom/api/v1/query" || (*queries)[0] != q {
				t.Fatalf("request %s %s query %q", r.Method, r.URL.Path, (*queries)[0])
			}
		})
	}
}

// NaN and the infinities are strings in the API, and decode as such (Evaluate then fails them).
func TestPrometheusSpecialValues(t *testing.T) {
	for body, check := range map[string]func(float64) bool{
		`"NaN"`:  math.IsNaN,
		`"+Inf"`: func(v float64) bool { return math.IsInf(v, 1) },
		`"-Inf"`: func(v float64) bool { return math.IsInf(v, -1) },
	} {
		p, _, _ := prometheus(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,`+body+`]}]}}`)
		r, err := p.Query(context.Background(), "q")
		if err != nil || len(r.Values) != 1 || !check(r.Values[0]) {
			t.Fatalf("%s: %+v, %v", body, r, err)
		}
	}
}

func TestPrometheusErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad query", http.StatusBadRequest, `{"status":"error","errorType":"bad_data","error":"1:5: parse error: unexpected end of input"}`,
			"Prometheus: bad_data: 1:5: parse error: unexpected end of input"},
		{"timeout", http.StatusServiceUnavailable, `{"status":"error","errorType":"timeout","error":"query timed out in expression evaluation"}`,
			"Prometheus: timeout: query timed out"},
		{"not the API", http.StatusBadGateway, `<html>Bad Gateway</html>`, "HTTP 502 from Prometheus, not an API response"},
		{"no envelope", http.StatusOK, `{}`, "HTTP 200 from Prometheus, not an API response"},
		{"success with an error code", http.StatusInternalServerError, `{"status":"success","data":{"resultType":"vector","result":[]}}`, "HTTP 500 from Prometheus"},
		{"histogram sample", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"histogram":[1,{"count":"2","sum":"3"}]}]}}`,
			"a vector sample with no value"},
		{"bad value", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`,
			`a sample value that isn't a number: "abc"`},
		{"value not a string", http.StatusOK, `{"status":"success","data":{"resultType":"scalar","result":[1,0.5]}}`,
			"a sample value that isn't a string"},
		{"short sample", http.StatusOK, `{"status":"success","data":{"resultType":"scalar","result":[1]}}`,
			"a sample that isn't [time, value]"},
		{"too large", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[` + strings.Repeat(`{"metric":{},"value":[1,"1"]},`, 40000) + `]}}`,
			"over 1048576 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := prometheus(t, tc.status, tc.body)
			r, err := p.Query(context.Background(), "q")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Query = %+v, %v; want an error containing %q", r, err, tc.want)
			}
		})
	}
}

func TestPrometheusUnreachableOrSlow(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	p, _ := NewPrometheus(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Query(ctx, "q"); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("slow: %v; want the context's deadline", err)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	p, _ = NewPrometheus(dead.URL, http.DefaultClient)
	if _, err := p.Query(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("closed: %v; want connection refused", err)
	}
}

func TestNewPrometheus(t *testing.T) {
	for base, want := range map[string]string{
		"http://prometheus.monitoring:9090":     "http://prometheus.monitoring:9090/api/v1/query",
		"https://example.com/prometheus/":       "https://example.com/prometheus/api/v1/query",
		"http://[::1]:9090":                     "http://[::1]:9090/api/v1/query",
		"":                                      "want http:// or https://",
		"prometheus:9090":                       "want http:// or https://",
		"localhost:9090":                        "want http:// or https://",
		"ftp://example.com":                     "want http:// or https://",
		"http://":                               "has no host",
		"http:///api":                           "has no host",
		"http://example.com/?x=1":               "want no query",
		"http://someone@example.com":            "want no query, fragment or user",
		"http://example.com/#frag":              "want no query",
		"http://example.com:9090/path with sp%": "invalid URL escape",
	} {
		p, err := NewPrometheus(base, http.DefaultClient)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			got = p.endpoint
		}
		if !strings.Contains(got, want) {
			t.Errorf("NewPrometheus(%q) = %q; want %q", base, got, want)
		}
	}
}
