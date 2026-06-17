package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/config"
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

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{MaxUploadBytes: 16 << 20, SampleSize: 40, FetchConcurrency: 8}
	oc, err := cache.NewObjectCache("")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, nil, store.NewUploadSource(), cache.New(), oc)
	return srv.Handler()
}

func uploadProfile(t *testing.T, h http.Handler, date, buildTag string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("date", date)
	_ = mw.WriteField("buildTag", buildTag)
	fw, err := mw.CreateFormFile("files", "p.cpuprofile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(sampleProfile)); err != nil {
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

func TestHealth(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
}
