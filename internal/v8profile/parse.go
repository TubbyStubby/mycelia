package v8profile

import (
	"fmt"
	"io"

	json "github.com/goccy/go-json"
)

// ParseProfile decodes a V8 CPU profile from r. It normalizes the legacy nested
// "head" form into the flat Nodes representation and tolerates a samples /
// timeDeltas length mismatch by truncating both to the shorter length.
func ParseProfile(r io.Reader) (*Profile, error) {
	var p Profile
	dec := json.NewDecoder(r)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode cpuprofile: %w", err)
	}

	if len(p.Nodes) == 0 && p.Head != nil {
		p.Nodes = flattenHead(p.Head)
		// Legacy form has no samples/timeDeltas; aggregation falls back to
		// hitCount-based timing.
		p.Samples = nil
		p.TimeDeltas = nil
	}

	if len(p.Nodes) == 0 {
		return nil, fmt.Errorf("cpuprofile has no nodes")
	}

	// Tolerate off-by-one / truncated profiles: keep samples and timeDeltas the
	// same length.
	if len(p.Samples) != len(p.TimeDeltas) {
		n := min(len(p.Samples), len(p.TimeDeltas))
		p.Samples = p.Samples[:n]
		p.TimeDeltas = p.TimeDeltas[:n]
	}

	// Drop a context block that does not line up with the (possibly truncated)
	// samples, so aggregation can trust the parallel indexing.
	if p.Async != nil && len(p.Async.Samples) != len(p.Samples) {
		if len(p.Async.Samples) > len(p.Samples) {
			p.Async.Samples = p.Async.Samples[:len(p.Samples)]
		} else {
			p.Async = nil
		}
	}

	return &p, nil
}

// flattenHead converts the legacy nested call tree into a flat node slice,
// preserving ids where present and assigning synthetic ids otherwise.
func flattenHead(head *legacyHead) []Node {
	var nodes []Node
	nextID := 1

	var walk func(h *legacyHead) int
	walk = func(h *legacyHead) int {
		id := h.ID
		if id == 0 {
			id = nextID
			nextID++
		} else if id >= nextID {
			nextID = id + 1
		}

		// The legacy form sometimes carries the call-frame fields inline rather
		// than under callFrame; prefer callFrame, fall back to inline.
		cf := h.CallFrame
		if cf.FunctionName == "" && cf.URL == "" {
			cf = CallFrame{
				FunctionName: h.FunctionName,
				ScriptID:     h.ScriptID,
				URL:          h.URL,
				LineNumber:   h.LineNumber,
				ColumnNumber: h.ColumnNumber,
			}
		}

		node := Node{ID: id, CallFrame: cf, HitCount: h.HitCount}
		idx := len(nodes)
		nodes = append(nodes, node)

		for i := range h.Children {
			childID := walk(&h.Children[i])
			nodes[idx].Children = append(nodes[idx].Children, childID)
		}
		return id
	}

	walk(head)
	return nodes
}
