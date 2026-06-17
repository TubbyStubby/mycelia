package v8profile

import "testing"

func TestDerivePackage(t *testing.T) {
	cases := []struct {
		url      string
		fn       string
		wantKind PackageKind
		wantName string
	}{
		{"file:///app/node_modules/express/lib/router/index.js", "handle", PkgNpm, "express"},
		{"file:///app/node_modules/@nestjs/core/router.js", "route", PkgNpm, "@nestjs/core"},
		{"/usr/src/app/node_modules/lodash/node_modules/foo/x.js", "f", PkgNpm, "foo"},
		{"node:internal/streams/readable", "read", PkgBuiltin, "node:builtin"},
		{"internal/process/task_queues.js", "tick", PkgBuiltin, "node:builtin"},
		{"", "(garbage collector)", PkgNative, "(garbage collector)"},
		{"", "", PkgNative, "(program)"},
		{"", "someNative", PkgNative, "(native)"},
		{"webpack://app/./src/index.js", "main", PkgEval, "(eval)"},
		{"/usr/src/app/src/services/user.js", "find", PkgApp, "app:src/services"},
		{"/usr/src/app/dist/controllers/x.js", "handle", PkgApp, "app:controllers"},
		{"file:///home/me/proj/lib/util.js", "u", PkgApp, "app:home"},
	}
	for _, c := range cases {
		kind, name := DerivePackage(c.url, c.fn)
		if kind != c.wantKind || name != c.wantName {
			t.Errorf("DerivePackage(%q,%q) = (%v,%q), want (%v,%q)",
				c.url, c.fn, kind, name, c.wantKind, c.wantName)
		}
	}
}
