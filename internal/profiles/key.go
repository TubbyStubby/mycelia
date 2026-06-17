package profiles

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ProfilesSegment is the fixed path token that always follows rootPath.
const ProfilesSegment = "profiles/"

// ProfilePrefix returns the GCS object prefix for the given root path, i.e.
// rootPath + "profiles/". rootPath is treated as an opaque prefix and is not
// modified (it may or may not end in a slash, matching the auto-profiler).
func ProfilePrefix(rootPath string) string {
	return rootPath + ProfilesSegment
}

// GroupPrefix returns the object prefix that contains exactly the members of a
// group.
func GroupPrefix(rootPath string, id GroupID) string {
	return ProfilePrefix(rootPath) + id.Env + "/" + id.Service + "/" + id.Date + "/" + id.BuildTag + "/"
}

// ParseObjectKey parses a full GCS object name into its components. rootPath is
// the configured (opaque) root prefix; it is stripped before parsing.
func ParseObjectKey(rootPath, name string) (ObjectKey, error) {
	k := ObjectKey{RootPath: rootPath, Raw: name}

	prefix := ProfilePrefix(rootPath)
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return k, fmt.Errorf("object %q does not start with prefix %q", name, prefix)
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 5 {
		return k, fmt.Errorf("object %q has %d path segments after prefix, want 5", name, len(parts))
	}
	k.Env, k.Service, k.Date, k.BuildTag = parts[0], parts[1], parts[2], parts[3]
	filename := parts[4]

	if !validDate(k.Date) {
		return k, fmt.Errorf("object %q has invalid date segment %q", name, k.Date)
	}

	ts, host, pid, err := parseFilename(filename)
	if err != nil {
		return k, fmt.Errorf("object %q: %w", name, err)
	}
	k.Timestamp, k.Hostname, k.PID = ts, host, pid
	return k, nil
}

// GroupID returns the group identity for this object.
func (k ObjectKey) GroupID() GroupID {
	return GroupID{Env: k.Env, Service: k.Service, Date: k.Date, BuildTag: k.BuildTag}
}

// parseFilename parses "{ms}_{host}_{pid}.cpuprofile". hostname may contain '_',
// so the timestamp is taken from before the first '_' and the pid from after the
// last '_'.
func parseFilename(filename string) (time.Time, string, int, error) {
	base, ok := strings.CutSuffix(filename, ".cpuprofile")
	if !ok {
		return time.Time{}, "", 0, fmt.Errorf("filename %q missing .cpuprofile suffix", filename)
	}

	firstUnderscore := strings.IndexByte(base, '_')
	lastUnderscore := strings.LastIndexByte(base, '_')
	if firstUnderscore < 0 || lastUnderscore == firstUnderscore {
		return time.Time{}, "", 0, fmt.Errorf("filename %q is not {ms}_{host}_{pid}", filename)
	}

	msStr := base[:firstUnderscore]
	host := base[firstUnderscore+1 : lastUnderscore]
	pidStr := base[lastUnderscore+1:]

	ms, err := strconv.ParseInt(msStr, 10, 64)
	if err != nil {
		return time.Time{}, "", 0, fmt.Errorf("filename %q has invalid timestamp: %w", filename, err)
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return time.Time{}, "", 0, fmt.Errorf("filename %q has invalid pid: %w", filename, err)
	}

	return time.UnixMilli(ms).UTC(), host, pid, nil
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
