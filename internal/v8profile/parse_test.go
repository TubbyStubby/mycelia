package v8profile

import (
	"strings"
	"testing"
)

func TestParseFlatProfile(t *testing.T) {
	const js = `{
		"nodes":[
			{"id":1,"callFrame":{"functionName":"(root)","scriptId":"0","url":"","lineNumber":-1,"columnNumber":-1},"hitCount":0,"children":[2]},
			{"id":2,"callFrame":{"functionName":"work","scriptId":"5","url":"file:///app/x.js","lineNumber":10,"columnNumber":2},"hitCount":3}
		],
		"startTime":1000,"endTime":4000,
		"samples":[2,2,2],
		"timeDeltas":[1000,1000,1000]
	}`
	p, err := ParseProfile(strings.NewReader(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Nodes) != 2 || len(p.Samples) != 3 {
		t.Fatalf("unexpected shape: %d nodes, %d samples", len(p.Nodes), len(p.Samples))
	}
	if p.Nodes[1].CallFrame.ScriptID != "5" {
		t.Errorf("scriptId = %q, want \"5\"", p.Nodes[1].CallFrame.ScriptID)
	}
	agg := AggregateProfile(p)
	if agg.Overall.SelfMicros != 3000 {
		t.Errorf("overall = %d, want 3000", agg.Overall.SelfMicros)
	}
}

func TestParseLengthMismatchTruncates(t *testing.T) {
	const js = `{
		"nodes":[{"id":1,"callFrame":{"functionName":"f","scriptId":"1","url":"a.js"},"hitCount":1}],
		"samples":[1,1,1],
		"timeDeltas":[5,5]
	}`
	p, err := ParseProfile(strings.NewReader(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Samples) != 2 || len(p.TimeDeltas) != 2 {
		t.Errorf("not truncated to 2: samples=%d timeDeltas=%d", len(p.Samples), len(p.TimeDeltas))
	}
}

func TestParseLegacyHead(t *testing.T) {
	const js = `{
		"head":{
			"id":1,"functionName":"(root)","url":"","hitCount":0,
			"children":[
				{"id":2,"functionName":"a","url":"file:///app/a.js","lineNumber":1,"hitCount":4,"children":[]}
			]
		}
	}`
	p, err := ParseProfile(strings.NewReader(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Nodes) != 2 {
		t.Fatalf("flattened nodes = %d, want 2", len(p.Nodes))
	}
	agg := AggregateProfile(p)
	if !agg.TimingApproximate {
		t.Errorf("legacy profile should be timingApproximate")
	}
	if agg.Overall.SelfSamples != 4 {
		t.Errorf("overall self samples = %d, want 4 (hitCount)", agg.Overall.SelfSamples)
	}
}
