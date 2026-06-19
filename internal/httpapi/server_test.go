package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/internal/store"
)

const sampleProfile = `{
	"nodes":[
		{"id":1,"callFrame":{"functionName":"(root)","scriptId":"0","url":""},"hitCount":0,"children":[2]},
		{"id":2,"callFrame":{"functionName":"hot","scriptId":"7","url":"file:///app/svc/h.js","lineNumber":3},"hitCount":2}
	],
	"startTime":0,"endTime":2,
	"samples":[2,2],
	"timeDeltas":[1,1]
}`

// asyncProfile carries an _async block so the breakdown contexts list (and its
// percentages) is populated. hot(2) runs under routes A (x2) and B (x1); cold(3)
// runs under A (x2). So hot's contexts are A=2µs, B=1µs (function total 3µs),
// while route A's own busy CPU is 4µs and B's is 1µs — making B the more
// lean-able owner of hot (100% of B vs 50% of A) despite owning fewer micros.
const asyncProfile = `{
	"nodes":[
		{"id":1,"callFrame":{"functionName":"(root)","scriptId":"0","url":""},"hitCount":0,"children":[2,3]},
		{"id":2,"callFrame":{"functionName":"hot","scriptId":"7","url":"file:///app/svc/h.js","lineNumber":3},"hitCount":3},
		{"id":3,"callFrame":{"functionName":"cold","scriptId":"7","url":"file:///app/svc/c.js","lineNumber":1},"hitCount":2}
	],
	"startTime":0,"endTime":5,
	"samples":[2,2,2,3,3],
	"timeDeltas":[1,1,1,1,1],
	"_async":{"version":1,"labels":["GET /a","GET /b"],"samples":[0,0,1,0,0]}
}`

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{MaxUploadBytes: 16 << 20, SampleSize: 40, FetchConcurrency: 8}
	oc, err := cache.NewObjectCache("")
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(cfg, nil, store.NewUploadSource(), cache.New(), oc)
	srv := New(cfg, eng)
	return srv.Handler()
}

func uploadProfile(t *testing.T, h http.Handler, date, buildTag string) {
	t.Helper()
	uploadBody(t, h, date, buildTag, sampleProfile)
}

func uploadBody(t *testing.T, h http.Handler, date, buildTag, profile string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("date", date)
	_ = mw.WriteField("buildTag", buildTag)
	fw, err := mw.CreateFormFile("files", "p.cpuprofile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(profile)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestUploadGroupCompareFlow(t *testing.T) {
	h := newTestServer(t)
	uploadProfile(t, h, "2024-01-01", "buildA")
	uploadProfile(t, h, "2024-01-02", "buildB")

	// Fetch one group's aggregation.
	req := httptest.NewRequest("GET", "/api/group/upload/manual/2024-01-01/buildA", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var gr groupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gr); err != nil {
		t.Fatal(err)
	}
	if gr.Agg == nil || gr.Agg.Overall.SelfMicros != 2 {
		t.Fatalf("group aggregation wrong: %+v", gr.Agg)
	}

	// Compare the two upload groups by function (NDJSON stream).
	body := `{"groups":[
		{"env":"upload","service":"manual","date":"2024-01-01","buildTag":"buildA"},
		{"env":"upload","service":"manual","date":"2024-01-02","buildTag":"buildB"}
	],"dimension":"function","metric":"selfMicros"}`
	req = httptest.NewRequest("POST", "/api/compare", strings.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("compare status = %d, body=%s", rec.Code, rec.Body.String())
	}

	m, sawProgress := readCompareStream(t, rec.Body.String())
	if !sawProgress {
		t.Errorf("expected at least one progress message")
	}
	if m == nil {
		t.Fatalf("no result message in stream:\n%s", rec.Body.String())
	}
	if len(m.Groups) != 2 {
		t.Fatalf("matrix groups = %d, want 2", len(m.Groups))
	}
	if len(m.Rows) == 0 {
		t.Fatalf("matrix has no rows")
	}
	for _, row := range m.Rows {
		if len(row.Cells) != 2 || len(row.Trend) != 2 {
			t.Errorf("row %q has %d cells / %d trend points, want 2/2", row.Display, len(row.Cells), len(row.Trend))
		}
	}
}

// readCompareStream parses an NDJSON compare stream, returning the result matrix
// and whether any progress message was seen.
func readCompareStream(t *testing.T, body string) (*compare.Matrix, bool) {
	t.Helper()
	var result *compare.Matrix
	sawProgress := false
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		var msg streamMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", line, err)
		}
		switch msg.Type {
		case "progress":
			sawProgress = true
		case "error":
			t.Fatalf("stream error: %s", msg.Error)
		case "result":
			result = msg.Matrix
		}
	}
	return result, sawProgress
}

func TestBreakdownEndpoint(t *testing.T) {
	h := newTestServer(t)
	uploadProfile(t, h, "2024-01-01", "buildA")

	// Discover the "hot" function key from the group aggregation.
	req := httptest.NewRequest("GET", "/api/group/upload/manual/2024-01-01/buildA", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var gr groupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gr); err != nil {
		t.Fatal(err)
	}
	var hotKey string
	for k, e := range gr.Agg.Functions {
		if strings.HasPrefix(e.Display, "hot") {
			hotKey = k
		}
	}
	if hotKey == "" {
		t.Fatalf("hot function not found in %+v", gr.Agg.Functions)
	}

	// Breakdown of hot: (root) should appear as a caller.
	req = httptest.NewRequest("GET", "/api/group/upload/manual/2024-01-01/buildA/breakdown?fn="+url.QueryEscape(hotKey), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("breakdown status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var bd compare.Breakdown
	if err := json.Unmarshal(rec.Body.Bytes(), &bd); err != nil {
		t.Fatal(err)
	}
	if bd.Key != hotKey {
		t.Errorf("breakdown key = %q, want %q", bd.Key, hotKey)
	}
	foundRoot := false
	for _, c := range bd.Callers {
		if strings.HasPrefix(c.Display, "(root)") {
			foundRoot = true
		}
	}
	if !foundRoot {
		t.Errorf("expected (root) among callers, got %+v", bd.Callers)
	}

	// Unknown function key -> 404.
	req = httptest.NewRequest("GET", "/api/group/upload/manual/2024-01-01/buildA/breakdown?fn=does-not-exist", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown-fn status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}

	// Missing fn parameter -> 400.
	req = httptest.NewRequest("GET", "/api/group/upload/manual/2024-01-01/buildA/breakdown", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no-fn status = %d, want 400", rec.Code)
	}
}

// TestBreakdownContextsEndpoint verifies the breakdown endpoint surfaces the
// per-context percentages and honours contextSort=pctOfContext.
func TestBreakdownContextsEndpoint(t *testing.T) {
	h := newTestServer(t)
	uploadBody(t, h, "2024-03-01", "buildC", asyncProfile)

	req := httptest.NewRequest("GET", "/api/group/upload/manual/2024-03-01/buildC", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var gr groupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gr); err != nil {
		t.Fatal(err)
	}
	var hotKey string
	for k, e := range gr.Agg.Functions {
		if strings.HasPrefix(e.Display, "hot") {
			hotKey = k
		}
	}
	if hotKey == "" {
		t.Fatalf("hot function not found in %+v", gr.Agg.Functions)
	}

	get := func(contextSort string) compare.Breakdown {
		t.Helper()
		u := "/api/group/upload/manual/2024-03-01/buildC/breakdown?fn=" + url.QueryEscape(hotKey)
		if contextSort != "" {
			u += "&contextSort=" + contextSort
		}
		req := httptest.NewRequest("GET", u, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("breakdown status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var bd compare.Breakdown
		if err := json.Unmarshal(rec.Body.Bytes(), &bd); err != nil {
			t.Fatal(err)
		}
		return bd
	}

	// Default (micros) order: A (2µs) before B (1µs), with the two percentages set.
	bd := get("")
	if len(bd.Contexts) != 2 {
		t.Fatalf("contexts = %+v, want 2", bd.Contexts)
	}
	byLabel := map[string]compare.BreakdownEdge{}
	for _, c := range bd.Contexts {
		byLabel[c.Display] = c
	}
	a, b := byLabel["GET /a"], byLabel["GET /b"]
	if a.PctOfContext != 50 || b.PctOfContext != 100 {
		t.Errorf("pctOfContext: A=%g B=%g, want 50 / 100", a.PctOfContext, b.PctOfContext)
	}
	if bd.Contexts[0].Display != "GET /a" {
		t.Errorf("default sort top = %q, want GET /a (more micros)", bd.Contexts[0].Display)
	}

	// Route-share order flips: B (100%) outranks A (50%).
	if top := get("pctOfContext").Contexts[0].Display; top != "GET /b" {
		t.Errorf("pctOfContext sort top = %q, want GET /b", top)
	}
}

func TestHealth(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
}
