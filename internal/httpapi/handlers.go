package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"gcsEnabled": s.gcs != nil,
	})
}

// handleGroups browses the env/service/date/buildTag hierarchy. When the
// "include" query equals "uploads" (default) it also merges in upload groups at
// the leaf level.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := profiles.GroupFilter{
		Env:      q.Get("env"),
		Service:  q.Get("service"),
		Date:     q.Get("date"),
		BuildTag: q.Get("buildTag"),
	}

	// Uploads-only view (no GCS configured, or explicitly requested).
	if filter.Env == store.UploadEnv || s.gcs == nil {
		res, err := s.uploads.Browse(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	res, err := s.gcs.Browse(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	// At the top level, surface uploads as a virtual environment so users can
	// reach uploaded groups through the same UI.
	if filter.Env == "" {
		if ups, _ := s.uploads.ListGroups(r.Context(), profiles.GroupFilter{}); len(ups) > 0 {
			res.Children = append(res.Children, store.UploadEnv)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	id := profiles.GroupID{
		Env:      r.PathValue("env"),
		Service:  r.PathValue("service"),
		Date:     r.PathValue("date"),
		BuildTag: r.PathValue("buildTag"),
	}

	agg, _, err := s.groupAggregation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	src, _ := s.sourceFor(id)
	var members []profiles.GroupMember
	if src != nil {
		if groups, err := src.ListGroups(r.Context(), profiles.GroupFilter{
			Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag,
		}); err == nil {
			for _, g := range groups {
				if g.ID == id {
					members = g.Members
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, groupResponse{ID: id, Members: members, Agg: agg})
}

// handleCompare streams NDJSON: zero or more {"type":"progress"} lines while
// profiles are processed, then a final {"type":"result"} (or {"type":"error"}).
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	var req compareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Groups) == 0 {
		writeError(w, http.StatusBadRequest, errBadRequest("at least one group is required"))
		return
	}
	if req.Dimension == "" {
		req.Dimension = compare.DimFunction
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	var encMu sync.Mutex
	send := func(m streamMsg) {
		encMu.Lock()
		defer encMu.Unlock()
		_ = enc.Encode(m)
		if flusher != nil {
			flusher.Flush()
		}
	}

	ctx := r.Context()

	// Plan: list + sample each group so we know the total work up front.
	plans := make([]groupPlan, 0, len(req.Groups))
	total := 0
	for _, id := range req.Groups {
		p, err := s.planGroup(ctx, id)
		if err != nil {
			send(streamMsg{Type: "error", Error: fmt.Sprintf("group %s: %v", id, err)})
			return
		}
		plans = append(plans, p)
		total += len(p.sampled)
	}

	prog := &progressReporter{total: total, emit: func(done, total int) {
		send(streamMsg{Type: "progress", Done: done, Total: total})
	}}
	send(streamMsg{Type: "progress", Done: 0, Total: total})

	// Build groups sequentially so concurrency stays bounded by
	// FetchConcurrency within each group and progress advances smoothly.
	aggs := make([]compare.GroupAggregation, len(plans))
	for i, p := range plans {
		agg, err := s.buildPlan(ctx, p, prog)
		if err != nil {
			send(streamMsg{Type: "error", Error: fmt.Sprintf("group %s: %v", p.id, err)})
			return
		}
		aggs[i] = compare.GroupAggregation{ID: p.id, Agg: agg, TotalProfiles: p.total}
	}

	matrix := compare.BuildMatrix(aggs, req.Dimension, req.Metric, req.TopN, req.allowedSet())
	send(streamMsg{Type: "result", Matrix: &matrix})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	id := profiles.GroupID{
		Service:  formOr(r, "service", "manual"),
		Date:     formOr(r, "date", "uploaded"),
		BuildTag: formOr(r, "buildTag", "upload"),
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, errBadRequest("no files uploaded under field \"files\""))
		return
	}

	var named []store.NamedBytes
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		buf, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		named = append(named, store.NamedBytes{Name: fh.Filename, Content: buf})
	}

	group, err := s.uploads.Add(id, named)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, uploadResponse{Group: group})
}

func formOr(r *http.Request, key, def string) string {
	if v := r.FormValue(key); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

type errBadRequest string

func (e errBadRequest) Error() string { return string(e) }
