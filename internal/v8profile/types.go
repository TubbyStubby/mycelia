// Package v8profile parses and aggregates V8 CPU profiles (.cpuprofile files)
// produced by the Node.js auto-profiler. These are JSON documents in the modern
// flat "nodes + samples + timeDeltas" format emitted by the V8 inspector's
// Profiler.stop, not Go pprof protobufs.
package v8profile

// CallFrame describes the source location of a profile node.
//
// Note: V8 serializes scriptId as a string, and lineNumber/columnNumber are
// 0-based.
type CallFrame struct {
	FunctionName string `json:"functionName"`
	ScriptID     string `json:"scriptId"`
	URL          string `json:"url"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

// PositionTick is per-line hit information (unused for aggregation, retained for
// completeness).
type PositionTick struct {
	Line  int `json:"line"`
	Ticks int `json:"ticks"`
}

// Node is one entry in the call tree. children references other node ids.
type Node struct {
	ID            int            `json:"id"`
	CallFrame     CallFrame      `json:"callFrame"`
	HitCount      int            `json:"hitCount"`
	Children      []int          `json:"children"`
	PositionTicks []PositionTick `json:"positionTicks,omitempty"`
	DeoptReason   string         `json:"deoptReason,omitempty"`
}

// legacyHead is the older nested call-tree form (no samples/timeDeltas). When
// present and Nodes is empty, parse.go flattens it into Nodes.
type legacyHead struct {
	ID           int          `json:"id"`
	CallFrame    CallFrame    `json:"callFrame"`
	FunctionName string       `json:"functionName"`
	ScriptID     string       `json:"scriptId"`
	URL          string       `json:"url"`
	LineNumber   int          `json:"lineNumber"`
	ColumnNumber int          `json:"columnNumber"`
	HitCount     int          `json:"hitCount"`
	Children     []legacyHead `json:"children"`
}

// Profile is a parsed V8 CPU profile.
type Profile struct {
	Nodes      []Node  `json:"nodes"`
	Samples    []int   `json:"samples"`
	TimeDeltas []int64 `json:"timeDeltas"` // microseconds between consecutive samples
	StartTime  int64   `json:"startTime"`  // microseconds
	EndTime    int64   `json:"endTime"`    // microseconds
	Title      string  `json:"title,omitempty"`

	// Head is only populated by the legacy nested form; consumers should use
	// Nodes after ParseProfile, which normalizes Head into Nodes.
	Head *legacyHead `json:"head,omitempty"`

	// Async is the optional context-attribution block emitted by the auto-profiler
	// (see examples/auto-profiler). Absent on profiles captured without context
	// capture; consumers must treat it as optional.
	Async *AsyncContext `json:"_async,omitempty"`
}

// AsyncContext attributes each CPU sample to the logical label (route/job/query
// name) that was active when it was taken. Samples is parallel to Profile.Samples;
// each entry indexes into Labels, or is -1 when the sample was not attributed.
type AsyncContext struct {
	Version int      `json:"version"`
	Labels  []string `json:"labels"`
	Samples []int    `json:"samples"`
}
