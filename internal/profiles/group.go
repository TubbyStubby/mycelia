// Package profiles models the auto-profiler's GCS object naming convention and
// groups profile objects by date + buildTag.
package profiles

import "time"

// ObjectKey is a parsed auto-profiler GCS object name.
//
// The auto-profiler writes keys as:
//
//	{rootPath}profiles/{env}/{service}/{date}/{buildTag}/{ms}_{host}_{pid}.cpuprofile
//
// rootPath is concatenated verbatim and has no guaranteed trailing slash.
type ObjectKey struct {
	RootPath  string    `json:"-"`
	Env       string    `json:"env"`
	Service   string    `json:"service"`
	Date      string    `json:"date"` // YYYY-MM-DD (UTC)
	BuildTag  string    `json:"buildTag"`
	Hostname  string    `json:"hostname"`
	PID       int       `json:"pid"`
	Timestamp time.Time `json:"timestamp"`
	Raw       string    `json:"raw"` // full GCS object name
}

// Group identifies a date + buildTag (within an env/service) group. Including
// env/service makes the identity unique and avoids cross-env collisions.
type GroupID struct {
	Env      string `json:"env"`
	Service  string `json:"service"`
	Date     string `json:"date"`
	BuildTag string `json:"buildTag"`
}

// String returns the canonical "env/service/date/buildTag" identity used as a
// cache and URL key.
func (g GroupID) String() string {
	return g.Env + "/" + g.Service + "/" + g.Date + "/" + g.BuildTag
}

// GroupMember is one object belonging to a group.
type GroupMember struct {
	Key  ObjectKey `json:"key"`
	Size int64     `json:"size"`
}

// Group is a date + buildTag group and its member objects.
type Group struct {
	ID      GroupID       `json:"id"`
	Members []GroupMember `json:"members"`
}

// GroupFilter narrows a group listing. Empty fields are wildcards.
type GroupFilter struct {
	Env      string
	Service  string
	Date     string
	BuildTag string
}
