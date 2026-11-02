package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Prometheus runs instant queries against a Prometheus server's HTTP API (POST /api/v1/query). Its address is the
// operator's --prometheus-url, never a rollout's field, so whoever can create a rollout can't point the operator's
// requests at an address of their choosing.
type Prometheus struct {
	endpoint string
	client   *http.Client
}

// NewPrometheus is a client for the Prometheus at base (http or https, with an optional path prefix such as
// http://monitoring/prometheus). The client's own timeout, if any, applies on top of each query's context.
func NewPrometheus(base string, client *http.Client) (*Prometheus, error) {
	u, err := url.Parse(base)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the Prometheus URL %q: %w", base, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("the Prometheus URL %q: want http:// or https://", base)
	case u.Host == "":
		return nil, fmt.Errorf("the Prometheus URL %q has no host", base)
	case strings.ContainsAny(base, "?#") || u.User != nil:
		// Checked on the text: a bare "?" or "#" leaves the parsed query and fragment empty, and would end up in the
		// endpoint ("http://p:9090?/api/v1/query" asks Prometheus's UI, not its API).
		return nil, fmt.Errorf("the Prometheus URL %q: want no query, fragment or user", base)
	}
	return &Prometheus{endpoint: strings.TrimSuffix(u.String(), "/") + "/api/v1/query", client: client}, nil
}

// maxResponse bounds the response read. An answer this large has far more than the one sample a check accepts.
const maxResponse = 1 << 20

// The API's envelope (https://prometheus.io/docs/prometheus/latest/querying/api/#format-overview).
type apiResponse struct {
	Status    string   `json:"status"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Warnings  []string `json:"warnings"`
	// IsPartial is how VictoriaMetrics' cluster version marks an answer missing some storage nodes' data.
	IsPartial bool `json:"isPartial"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Query runs one instant query, evaluated at Prometheus's own current time.
func (p *Prometheus) Query(ctx context.Context, query string) (Result, error) {
	form := url.Values{"query": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading Prometheus's response: %w", err)
	}
	if len(body) > maxResponse {
		return Result{}, fmt.Errorf("Prometheus's response is over %d bytes", maxResponse)
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Status == "" {
		return Result{}, fmt.Errorf("HTTP %d from Prometheus, not an API response", resp.StatusCode)
	}
	if r.Status != "success" {
		return Result{}, fmt.Errorf("Prometheus: %s: %s", r.ErrorType, r.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("HTTP %d from Prometheus", resp.StatusCode)
	}
	// A warning means the answer may be partial: a store or remote-read endpoint was down, a limit was hit (Thanos,
	// Cortex and Mimir answer "success" with the series they could get). A partial answer can leave out exactly the
	// failing series, so it fails. PromQL's info annotations ("PromQL info: ...", in warnings before Prometheus 3)
	// say nothing about the data and are ignored.
	if r.IsPartial {
		return Result{}, errors.New("Prometheus marked the answer partial (isPartial)")
	}
	for _, w := range r.Warnings {
		if !strings.HasPrefix(w, "PromQL info:") {
			return Result{}, fmt.Errorf("Prometheus answered with a warning, so the answer may be partial: %s", w)
		}
	}
	res := Result{Type: r.Data.ResultType}
	switch res.Type {
	case "scalar":
		v, err := sampleValue(r.Data.Result)
		if err != nil {
			return Result{}, err
		}
		res.Values = []float64{v}
	case "vector":
		var samples []struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &samples); err != nil {
			return Result{}, fmt.Errorf("decoding a vector: %w", err)
		}
		for _, s := range samples {
			if s.Value == nil {
				// A native histogram sample carries "histogram" instead of "value".
				return Result{}, errors.New("a vector sample with no value (a native histogram?)")
			}
			v, err := sampleValue(s.Value)
			if err != nil {
				return Result{}, err
			}
			res.Values = append(res.Values, v)
		}
	}
	return res, nil
}

// sampleValue decodes [<unix time>, "<value>"]; the value is a string so that NaN and the infinities fit in JSON.
func sampleValue(raw json.RawMessage) (float64, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return 0, fmt.Errorf("a sample that isn't [time, value]: %.100s", raw)
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, fmt.Errorf("a sample value that isn't a string: %.100s", pair[1])
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("a sample value that isn't a number: %q", s)
	}
	return v, nil
}
