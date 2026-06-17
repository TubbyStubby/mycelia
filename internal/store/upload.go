package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/TubbyStubby/mycelia/internal/profiles"
)

// UploadEnv is the synthetic env namespace used for manually uploaded groups so
// they can be routed away from GCS.
const UploadEnv = "upload"

// NamedBytes is an uploaded file's name and contents.
type NamedBytes struct {
	Name    string
	Content []byte
}

// UploadSource is an in-memory ProfileSource holding manually uploaded
// profiles, grouped by date + buildTag exactly like GCS groups.
type UploadSource struct {
	mu     sync.RWMutex
	groups map[string]*uploadGroup // key: GroupID.String()
}

type uploadGroup struct {
	id      profiles.GroupID
	members map[string][]byte // key: ObjectKey.Raw
	order   []profiles.GroupMember
}

// NewUploadSource creates an empty upload source.
func NewUploadSource() *UploadSource {
	return &UploadSource{groups: map[string]*uploadGroup{}}
}

// Add ingests files into the group identified by id (Env is forced to
// UploadEnv) and returns the updated group. Re-uploading into the same
// date+buildTag merges with existing members.
func (s *UploadSource) Add(id profiles.GroupID, files []NamedBytes) (profiles.Group, error) {
	if len(files) == 0 {
		return profiles.Group{}, fmt.Errorf("upload: no files provided")
	}
	id.Env = UploadEnv
	if id.Service == "" {
		id.Service = "manual"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	g := s.groups[id.String()]
	if g == nil {
		g = &uploadGroup{id: id, members: map[string][]byte{}}
		s.groups[id.String()] = g
	}

	for i, f := range files {
		raw := fmt.Sprintf("%s/%d_%s", id.String(), len(g.order)+i, f.Name)
		content := make([]byte, len(f.Content))
		copy(content, f.Content)
		g.members[raw] = content
		g.order = append(g.order, profiles.GroupMember{
			Key: profiles.ObjectKey{
				Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag,
				Hostname: f.Name, Raw: raw,
			},
			Size: int64(len(content)),
		})
	}

	return toGroup(g), nil
}

func toGroup(g *uploadGroup) profiles.Group {
	members := make([]profiles.GroupMember, len(g.order))
	copy(members, g.order)
	return profiles.Group{ID: g.id, Members: members}
}

// Browse lists upload groups. Uploads use a flat namespace, so Browse only
// meaningfully responds at the buildTag/leaf level; it returns all groups.
func (s *UploadSource) Browse(ctx context.Context, f profiles.GroupFilter) (BrowseResult, error) {
	groups, err := s.ListGroups(ctx, f)
	if err != nil {
		return BrowseResult{}, err
	}
	return BrowseResult{Level: LevelBuildTag, Groups: groups}, nil
}

// ListGroups returns the upload groups matching the filter.
func (s *UploadSource) ListGroups(ctx context.Context, f profiles.GroupFilter) ([]profiles.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var groups []profiles.Group
	for _, g := range s.groups {
		if f.Service != "" && g.id.Service != f.Service {
			continue
		}
		if f.Date != "" && g.id.Date != f.Date {
			continue
		}
		if f.BuildTag != "" && g.id.BuildTag != f.BuildTag {
			continue
		}
		groups = append(groups, toGroup(g))
	}
	sortGroups(groups)
	return groups, nil
}

// OpenMember returns the in-memory bytes for an uploaded member.
func (s *UploadSource) OpenMember(ctx context.Context, key profiles.ObjectKey) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	gid := key.GroupID()
	g := s.groups[gid.String()]
	if g == nil {
		return nil, fmt.Errorf("upload: group %q not found", gid)
	}
	content, ok := g.members[key.Raw]
	if !ok {
		return nil, fmt.Errorf("upload: member %q not found", key.Raw)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// Has reports whether a group belongs to this upload source.
func (s *UploadSource) Has(id profiles.GroupID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.groups[id.String()]
	return ok
}
