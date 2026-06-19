// Package httpapi wires the profile engine into an HTTP server with a JSON API
// and an embedded static frontend.
package httpapi

import (
	"net/http"

	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/web"
)

// Server holds the API dependencies.
type Server struct {
	cfg config.Config
	eng *engine.Engine
}

// New builds a Server around the shared profile engine.
func New(cfg config.Config, eng *engine.Engine) *Server {
	return &Server{cfg: cfg, eng: eng}
}

// Handler returns the configured HTTP handler (routes + static assets).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/groups", s.handleGroups)
	mux.HandleFunc("GET /api/group/{env}/{service}/{date}/{buildTag}", s.handleGroup)
	mux.HandleFunc("GET /api/group/{env}/{service}/{date}/{buildTag}/breakdown", s.handleBreakdown)
	mux.HandleFunc("POST /api/compare", s.handleCompare)
	mux.HandleFunc("POST /api/upload", s.handleUpload)

	mux.Handle("GET /", http.FileServerFS(web.FS()))
	return mux
}
