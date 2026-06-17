// Package store provides profile sources: a GCS-backed source following the
// auto-profiler naming convention, and an in-memory source for manual uploads.
package store

import (
	"context"
	"io"

	"github.com/TubbyStubby/mycelia/internal/profiles"
)

// Level identifies a step in the env -> service -> date -> buildTag drilldown.
type Level string

const (
	LevelEnv      Level = "env"
	LevelService  Level = "service"
	LevelDate     Level = "date"
	LevelBuildTag Level = "buildTag"
)

// BrowseResult is the result of browsing one level of the hierarchy. Either
// Children (the next-level segment names) or Groups (at the leaf) is populated.
type BrowseResult struct {
	Level    Level            `json:"level"`
	Children []string         `json:"children,omitempty"`
	Groups   []profiles.Group `json:"groups,omitempty"`
}

// ProfileSource loads profile groups and their member objects.
type ProfileSource interface {
	// Browse lists the next hierarchy level given an optional filter. With no
	// env it returns environments; with env+service+date it returns buildTags;
	// fully specified it returns the leaf group(s).
	Browse(ctx context.Context, filter profiles.GroupFilter) (BrowseResult, error)

	// ListGroups returns all groups matching the filter, each with its members.
	ListGroups(ctx context.Context, filter profiles.GroupFilter) ([]profiles.Group, error)

	// OpenMember opens the raw bytes of a single profile object.
	OpenMember(ctx context.Context, key profiles.ObjectKey) (io.ReadCloser, error)
}
