package v8profile

import "strings"

// PackageKind classifies the origin of a call frame's owning "package".
type PackageKind int

const (
	PkgNpm     PackageKind = iota // dependency under node_modules
	PkgApp                        // first-party application code
	PkgBuiltin                    // node: / internal Node.js modules
	PkgNative                     // V8 synthetic / native frames (empty url)
	PkgEval                       // eval / anonymous / bundler frames
)

const nodeModules = "node_modules/"

// Filter categories used by the UI toggles. Every entity belongs to exactly one.
const (
	CatNative      = "native"       // V8 synthetics, GC, program, node: builtins
	CatNodeModules = "node_modules" // npm dependencies
	CatUser        = "user"         // first-party app code and eval
	CatIdle        = "idle"         // the V8 "(idle)" frame
)

// Category maps a package kind + derived name to a filter category. Idle is
// carved out of native so it can be toggled independently (it dominates wall
// time in mostly-idle processes).
func Category(kind PackageKind, name string) string {
	switch kind {
	case PkgNpm:
		return CatNodeModules
	case PkgApp, PkgEval:
		return CatUser
	case PkgBuiltin:
		return CatNative
	default: // PkgNative
		if name == "(idle)" {
			return CatIdle
		}
		return CatNative
	}
}

// DerivePackage maps a call frame's url and function name to an owning package
// name and kind.
//
//   - node_modules/<pkg> (deepest wins, scoped @scope/pkg aware) -> PkgNpm
//   - node:* / internal/*                                        -> PkgBuiltin
//   - eval / anonymous / webpack                                 -> PkgEval
//   - empty url (V8 synthetics, native)                          -> PkgNative
//   - everything else (app code), grouped by top-level dir       -> PkgApp
func DerivePackage(url, functionName string) (PackageKind, string) {
	u := strings.ReplaceAll(url, "\\", "/")

	if u == "" {
		switch functionName {
		case "(root)", "(program)", "(idle)", "(garbage collector)", "(gc)":
			return PkgNative, functionName
		case "":
			return PkgNative, "(program)"
		default:
			return PkgNative, "(native)"
		}
	}

	if strings.HasPrefix(u, "node:") || strings.HasPrefix(u, "internal/") {
		return PkgBuiltin, "node:builtin"
	}

	if idx := strings.LastIndex(u, nodeModules); idx >= 0 {
		rest := u[idx+len(nodeModules):]
		return PkgNpm, npmPackageName(rest)
	}

	if isEvalOrBundler(u) {
		return PkgEval, "(eval)"
	}

	return PkgApp, "app:" + appTopDir(u)
}

// npmPackageName extracts the package name from a path that follows a
// node_modules/ segment, handling scoped packages (@scope/pkg).
func npmPackageName(rest string) string {
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "(unknown)"
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) >= 2 && parts[1] != "" {
			return parts[0] + "/" + parts[1]
		}
		return parts[0]
	}
	return parts[0]
}

func isEvalOrBundler(u string) bool {
	return strings.Contains(u, "eval at ") ||
		strings.HasPrefix(u, "evalmachine.") ||
		strings.HasPrefix(u, "webpack://") ||
		strings.HasPrefix(u, "VM")
}

// appTopDir returns the top-level source directory for first-party code,
// stripping common scheme/prefix noise so the grouping is stable across hosts.
func appTopDir(u string) string {
	u = strings.TrimPrefix(u, "file://")

	// Anchor on common container/source roots so the directory after them is
	// the meaningful top-level package (e.g. /usr/src/app/src/foo -> src).
	for _, anchor := range []string{"/dist/", "/build/", "/src/", "/app/"} {
		if i := strings.LastIndex(u, anchor); i >= 0 {
			tail := u[i+len(anchor):]
			seg := firstSegment(tail)
			if seg == "" {
				continue
			}
			anchorName := strings.Trim(anchor, "/")
			// "." means the file sits directly under the anchor (no subdir).
			if seg == "." {
				return anchorName
			}
			if anchor == "/src/" {
				// For /src/, keep the source sub-directory (e.g. src/services).
				return "src/" + seg
			}
			return seg
		}
	}

	// Fall back to the first non-empty path segment.
	trimmed := strings.TrimPrefix(u, "/")
	if seg := firstSegment(trimmed); seg != "" {
		return seg
	}
	return "(app)"
}

// firstSegment returns the first path segment, or the whole string if it has no
// slash. If the result still contains no directory (a bare filename), it
// returns "." to denote the root directory.
func firstSegment(p string) string {
	if p == "" {
		return ""
	}
	if before, _, found := strings.Cut(p, "/"); found {
		return before
	}
	// Bare filename, no directory component.
	return "."
}
