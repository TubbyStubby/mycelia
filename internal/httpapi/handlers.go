package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"gcsEnabled": s.eng.GCSEnabled(),
	})
}

// handleGroups browses the env/service/date/buildTag hierarchy, also surfacing
// uploaded groups (as a virtual env at the top level, or directly when GCS is
// not configured).
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := profiles.GroupFilter{
		Env:      q.Get("env"),
		Service:  q.Get("service"),
		Date:     q.Get("date"),
		BuildTag: q.Get("buildTag"),
	}

	res, err := s.eng.Browse(r.Context(), filter, true)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
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

	agg, _, err := s.eng.GroupAggregation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	members, _ := s.eng.Members(r.Context(), id)

	writeJSON(w, http.StatusOK, groupResponse{ID: id, Members: members, Agg: agg})
}

// handleBreakdown drills one entity within a single group. ?key= (or the legacy
// ?fn=) is the entity's key from the group/compare views; ?dim= selects the
// dimension (function|package|file|context, default function); ?topN= caps each
// list (default 25); ?stitch=false disables async/native trampoline stitching of
// callers (function dimension only).
func (s *Server) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	id := profiles.GroupID{
		Env:      r.PathValue("env"),
		Service:  r.PathValue("service"),
		Date:     r.PathValue("date"),
		BuildTag: r.PathValue("buildTag"),
	}

	q := r.URL.Query()
	key := q.Get("key")
	if key == "" {
		key = q.Get("fn") // legacy parameter name
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, errBadRequest(`query parameter "key" is required`))
		return
	}
	dim := compare.Dimension(q.Get("dim"))
	if dim == "" {
		dim = compare.DimFunction
	}
	if !validDimension(dim) {
		writeError(w, http.StatusBadRequest, errBadRequest("invalid dim "+strconv.Quote(string(dim))))
		return
	}
	topN := 25
	if v := q.Get("topN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			topN = n
		}
	}
	stitch := q.Get("stitch") != "false" // default on
	// contextSort orders the Contexts list; anything but "pctOfContext" is micros.
	ctxSort := compare.CtxSortMicros
	if q.Get("contextSort") == string(compare.CtxSortPctOfContext) {
		ctxSort = compare.CtxSortPctOfContext
	}

	bd, err := s.eng.EntityBreakdown(r.Context(), id, dim, key, topN, stitch, ctxSort)
	if err != nil {
		// A missing entity is a client-level miss; upstream fetch errors are not.
		if errors.Is(err, engine.ErrEntityNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, bd)
}

// validDimension reports whether dim is one of the breakdownable dimensions.
// Overall has no per-entity breakdown.
func validDimension(dim compare.Dimension) bool {
	switch dim {
	case compare.DimFunction, compare.DimPackage, compare.DimFile, compare.DimContext:
		return true
	default:
		return false
	}
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

	prog := engine.NewProgressReporter(0, func(done, total int) {
		send(streamMsg{Type: "progress", Done: done, Total: total})
	})

	matrix, err := s.eng.Compare(r.Context(), req.Groups, engine.CompareOptions{
		Dimension:  req.Dimension,
		Metric:     req.Metric,
		TopN:       req.TopN,
		Categories: req.Categories,
		Sort:       req.Sort,
	}, prog)
	if err != nil {
		send(streamMsg{Type: "error", Error: err.Error()})
		return
	}
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

	group, err := s.eng.AddUpload(id, named)
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
