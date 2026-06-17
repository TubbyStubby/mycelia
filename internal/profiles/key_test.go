package profiles

import (
	"testing"
	"time"
)

// makeKey reproduces the auto-profiler's object-name construction.
func makeKey(rootPath, env, service, date, buildTag string, ms int64, host string, pid int) string {
	return rootPath + "profiles/" + env + "/" + service + "/" + date + "/" + buildTag + "/" +
		itoa(ms) + "_" + host + "_" + itoa(int64(pid)) + ".cpuprofile"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestParseObjectKeyRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		rootPath string
		env      string
		service  string
		date     string
		buildTag string
		ms       int64
		host     string
		pid      int
	}{
		{"simple", "", "prod", "api", "2024-06-05", "v1.2.3", 1717608923000, "api-prod-01", 12847},
		{"root-no-slash", "tenants/acme", "prod", "web", "2025-01-02", "build-456", 1700000000000, "host01", 42},
		{"host-with-underscores", "", "staging", "svc", "2025-12-31", "abc1234-567", 1700000001234, "pod_abc_123", 9},
		{"buildtag-with-hyphens", "root/", "prod", "worker", "2026-02-02", "v1.2.3-rc.1-build-99", 1750000000000, "w1", 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := makeKey(c.rootPath, c.env, c.service, c.date, c.buildTag, c.ms, c.host, c.pid)
			k, err := ParseObjectKey(c.rootPath, name)
			if err != nil {
				t.Fatalf("ParseObjectKey: %v", err)
			}
			if k.Env != c.env || k.Service != c.service || k.Date != c.date || k.BuildTag != c.buildTag {
				t.Errorf("group fields = %q/%q/%q/%q, want %q/%q/%q/%q",
					k.Env, k.Service, k.Date, k.BuildTag, c.env, c.service, c.date, c.buildTag)
			}
			if k.Hostname != c.host {
				t.Errorf("hostname = %q, want %q", k.Hostname, c.host)
			}
			if k.PID != c.pid {
				t.Errorf("pid = %d, want %d", k.PID, c.pid)
			}
			if want := time.UnixMilli(c.ms).UTC(); !k.Timestamp.Equal(want) {
				t.Errorf("timestamp = %v, want %v", k.Timestamp, want)
			}
			if k.GroupID() != (GroupID{c.env, c.service, c.date, c.buildTag}) {
				t.Errorf("group id mismatch: %v", k.GroupID())
			}
		})
	}
}

func TestParseObjectKeyRejectsBadInput(t *testing.T) {
	bad := []string{
		"profiles/prod/api/notadate/build/1_h_2.cpuprofile",
		"profiles/prod/api/2024-06-05/build/missing-underscores.cpuprofile",
		"wrongprefix/prod/api/2024-06-05/build/1_h_2.cpuprofile",
		"profiles/prod/api/2024-06-05/build/1_h_2.txt",
	}
	for _, name := range bad {
		if _, err := ParseObjectKey("", name); err == nil {
			t.Errorf("ParseObjectKey(%q) = nil error, want error", name)
		}
	}
}
