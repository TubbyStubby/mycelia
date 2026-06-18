package httpapi

import (
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// compareRequest is the body of POST /api/compare.
type compareRequest struct {
	Groups     []profiles.GroupID `json:"groups"`
	Dimension  compare.Dimension  `json:"dimension"`
	Metric     compare.Metric     `json:"metric"`
	TopN       int                `json:"topN"`
	Categories []string           `json:"categories"` // enabled filter categories; empty = all
	Sort       compare.SortMode   `json:"sort"`       // max (default) | delta | deltaPct
}

// streamMsg is one NDJSON line emitted by the streaming compare endpoint.
// Done/Total are always serialized (not omitempty) so a 0/N progress message
// reaches the client intact.
type streamMsg struct {
	Type   string          `json:"type"` // "progress" | "result" | "error"
	Done   int             `json:"done"`
	Total  int             `json:"total"`
	Error  string          `json:"error,omitempty"`
	Matrix *compare.Matrix `json:"matrix,omitempty"`
}

// groupResponse is returned by GET /api/group/...
type groupResponse struct {
	ID      profiles.GroupID       `json:"id"`
	Members []profiles.GroupMember `json:"members"`
	Agg     *v8profile.Aggregation `json:"aggregation"`
}

// uploadResponse is returned by POST /api/upload.
type uploadResponse struct {
	Group profiles.Group `json:"group"`
}

// errorResponse is the JSON error envelope.
type errorResponse struct {
	Error string `json:"error"`
}
