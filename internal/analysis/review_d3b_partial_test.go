package analysis

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Review of day 3, second round. 26a4e07 fails a "success" answer that may be partial, but only when it carries
// "warnings". VictoriaMetrics (cluster vmselect, which serves the same /api/v1/query) marks a partial answer, some
// vmstorage nodes unreachable, with "isPartial": true and no warnings, so the series on the missing nodes (possibly
// exactly the failing ones) are left out and the check still passes.
func TestReviewD3bAPartialAnswerWithoutWarningsFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","isPartial":true,"data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.001"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if res, err := p.Query(context.Background(), "up"); err == nil {
		t.Fatalf("Query = %+v, nil for an answer marked isPartial; want it failed like one with warnings", res)
	}
}
