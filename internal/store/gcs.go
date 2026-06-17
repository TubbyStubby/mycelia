package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/TubbyStubby/mycelia/internal/profiles"
)

// GCSSource reads auto-profiler profiles from a GCS bucket using a
// service-account key file.
type GCSSource struct {
	client   *storage.Client
	bucket   *storage.BucketHandle
	rootPath string
}

// NewGCSSource builds a GCS-backed source. keyFile must point to a
// service-account JSON key.
func NewGCSSource(ctx context.Context, bucket, keyFile, rootPath string) (*GCSSource, error) {
	if bucket == "" {
		return nil, errors.New("gcs: bucket name is required")
	}
	if keyFile == "" {
		return nil, errors.New("gcs: service-account key file is required")
	}
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		CredentialsFile: keyFile,
		Scopes:          []string{storage.ScopeReadOnly},
	})
	if err != nil {
		return nil, fmt.Errorf("gcs: load credentials from key file: %w", err)
	}
	client, err := storage.NewClient(ctx, option.WithAuthCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("gcs: new client: %w", err)
	}
	return &GCSSource{
		client:   client,
		bucket:   client.Bucket(bucket),
		rootPath: rootPath,
	}, nil
}

// Close releases the underlying GCS client.
func (s *GCSSource) Close() error { return s.client.Close() }

// Browse lists the next level of the env/service/date/buildTag hierarchy using
// delimiter-based prefix listing.
func (s *GCSSource) Browse(ctx context.Context, f profiles.GroupFilter) (BrowseResult, error) {
	prefix := profiles.ProfilePrefix(s.rootPath)
	level := LevelEnv

	switch {
	case f.Env == "":
		level = LevelEnv
	case f.Service == "":
		prefix += f.Env + "/"
		level = LevelService
	case f.Date == "":
		prefix += f.Env + "/" + f.Service + "/"
		level = LevelDate
	case f.BuildTag == "":
		prefix += f.Env + "/" + f.Service + "/" + f.Date + "/"
		level = LevelBuildTag
	default:
		// Fully specified: return the single leaf group.
		groups, err := s.ListGroups(ctx, f)
		if err != nil {
			return BrowseResult{}, err
		}
		return BrowseResult{Level: LevelBuildTag, Groups: groups}, nil
	}

	children, err := s.listPrefixes(ctx, prefix)
	if err != nil {
		return BrowseResult{}, err
	}

	// At the buildTag level, expose the leaf groups with their member counts.
	if level == LevelBuildTag {
		groups := make([]profiles.Group, 0, len(children))
		for _, bt := range children {
			id := profiles.GroupID{Env: f.Env, Service: f.Service, Date: f.Date, BuildTag: bt}
			g, err := s.loadGroup(ctx, id)
			if err != nil {
				return BrowseResult{}, err
			}
			groups = append(groups, g)
		}
		return BrowseResult{Level: level, Groups: groups}, nil
	}

	return BrowseResult{Level: level, Children: children}, nil
}

// listPrefixes returns the immediate "subdirectory" segment names under prefix.
func (s *GCSSource) listPrefixes(ctx context.Context, prefix string) ([]string, error) {
	q := &storage.Query{Prefix: prefix, Delimiter: "/"}
	if err := q.SetAttrSelection([]string{"Name", "Size"}); err != nil {
		return nil, err
	}
	it := s.bucket.Objects(ctx, q)
	var out []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list %q: %w", prefix, err)
		}
		if attrs.Prefix != "" {
			seg := strings.TrimSuffix(strings.TrimPrefix(attrs.Prefix, prefix), "/")
			if seg != "" {
				out = append(out, seg)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListGroups returns all groups matching the filter.
func (s *GCSSource) ListGroups(ctx context.Context, f profiles.GroupFilter) ([]profiles.Group, error) {
	prefix := profiles.ProfilePrefix(s.rootPath)
	prefix += joinFilter(f)

	q := &storage.Query{Prefix: prefix}
	if err := q.SetAttrSelection([]string{"Name", "Size"}); err != nil {
		return nil, err
	}
	it := s.bucket.Objects(ctx, q)

	byGroup := map[string]*profiles.Group{}
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list %q: %w", prefix, err)
		}
		if !strings.HasSuffix(attrs.Name, ".cpuprofile") {
			continue
		}
		key, err := profiles.ParseObjectKey(s.rootPath, attrs.Name)
		if err != nil {
			continue // ignore objects that don't match the convention
		}
		id := key.GroupID()
		g := byGroup[id.String()]
		if g == nil {
			g = &profiles.Group{ID: id}
			byGroup[id.String()] = g
		}
		g.Members = append(g.Members, profiles.GroupMember{Key: key, Size: attrs.Size})
	}

	groups := make([]profiles.Group, 0, len(byGroup))
	for _, g := range byGroup {
		groups = append(groups, *g)
	}
	sortGroups(groups)
	return groups, nil
}

// loadGroup lists members for a single fully-specified group.
func (s *GCSSource) loadGroup(ctx context.Context, id profiles.GroupID) (profiles.Group, error) {
	f := profiles.GroupFilter{Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag}
	groups, err := s.ListGroups(ctx, f)
	if err != nil {
		return profiles.Group{}, err
	}
	if len(groups) == 0 {
		return profiles.Group{ID: id}, nil
	}
	return groups[0], nil
}

// OpenMember streams a profile object's bytes.
func (s *GCSSource) OpenMember(ctx context.Context, key profiles.ObjectKey) (io.ReadCloser, error) {
	rc, err := s.bucket.Object(key.Raw).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: open %q: %w", key.Raw, err)
	}
	return rc, nil
}

// joinFilter builds the part of the prefix that follows "profiles/" for the
// given filter, stopping at the first empty field.
func joinFilter(f profiles.GroupFilter) string {
	var b strings.Builder
	for _, seg := range []string{f.Env, f.Service, f.Date, f.BuildTag} {
		if seg == "" {
			break
		}
		b.WriteString(seg)
		b.WriteByte('/')
	}
	return b.String()
}

func sortGroups(groups []profiles.Group) {
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i].ID, groups[j].ID
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.BuildTag != b.BuildTag {
			return a.BuildTag < b.BuildTag
		}
		if a.Env != b.Env {
			return a.Env < b.Env
		}
		return a.Service < b.Service
	})
}
