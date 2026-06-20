package v8profile

import "strconv"

// FormatVersion identifies the aggregation format produced by AggregateProfile
// and MergeAggregations (both the Aggregation struct shape and the meaning of
// its values). Bump it whenever either changes, so the object cache does not
// reuse aggregations built by an older binary. The cache partitions entries by
// this version — in the cache key and under a v<N>/ subdirectory on disk — so a
// bump strands the old entries (inert, manually removable) and recomputes.
//
// History:
//
//	1 — initial function/file/package self+total aggregation.
//	2 — added the function call-graph Edges map.
//	3 — added async context attribution (Contexts + FunctionContexts).
//	4 — added Entity.File and per-context package/file self attribution
//	    (ContextPackages + ContextFiles).
const FormatVersion = 4

// Metric holds self and inclusive (total) cost for an entity, in both sample
// counts and microseconds.
type Metric struct {
	SelfSamples  int64 `json:"selfSamples"`
	TotalSamples int64 `json:"totalSamples"`
	SelfMicros   int64 `json:"selfMicros"`
	TotalMicros  int64 `json:"totalMicros"`
}

func (m *Metric) add(o Metric) {
	m.SelfSamples += o.SelfSamples
	m.TotalSamples += o.TotalSamples
	m.SelfMicros += o.SelfMicros
	m.TotalMicros += o.TotalMicros
}

// EntityKind distinguishes the aggregation granularity.
type EntityKind int

const (
	KindFunction EntityKind = iota
	KindFile
	KindPackage
	KindContext
)

// Entity is an aggregated function, file, or package row.
type Entity struct {
	Key      string     `json:"key"`
	Display  string     `json:"display"`
	Kind     EntityKind `json:"kind"`
	Package  string     `json:"package"`
	Category string     `json:"category"` // native|node_modules|user|idle
	// File is the owning file (script URL) of a function entity, so a file's
	// member functions can be listed exactly. Set only for KindFunction; empty
	// for file/package/context entities.
	File   string `json:"file,omitempty"`
	Metric Metric `json:"metric"`
}

// Aggregation is the per-profile (or merged per-group) rollup across all three
// granularities.
type Aggregation struct {
	Functions map[string]*Entity `json:"functions"`
	Files     map[string]*Entity `json:"files"`
	Packages  map[string]*Entity `json:"packages"`

	// Edges holds the function call graph as caller key -> callee key -> summed
	// inclusive (subtree) cost, enabling caller/callee breakdowns. Built at the
	// node-edge level, so recursion needs no special collapse here.
	Edges map[string]map[string]Metric `json:"edges,omitempty"`

	// Contexts attributes self time to the logical label (route/job) that was
	// active per sample (from Profile.Async). Self == Total at this level; nil
	// when the profile carried no context block.
	Contexts map[string]*Entity `json:"contexts,omitempty"`

	// FunctionContexts maps a function key -> context label -> its inclusive
	// (recursion-collapsed) cost under that function, so a breakdown can show
	// which logical owners drive a hot function. nil without a context block.
	FunctionContexts map[string]map[string]Metric `json:"functionContexts,omitempty"`

	// ContextPackages and ContextFiles attribute each context label's self time
	// to the package / file of the sampled (leaf) frame: label -> package-or-file
	// key -> self (Self == Total here, as for Contexts). Summing over packages
	// (or files) for a label recovers that label's Contexts self, so they cleanly
	// answer "where does this route's CPU go". nil without a context block.
	ContextPackages map[string]map[string]Metric `json:"contextPackages,omitempty"`
	ContextFiles    map[string]map[string]Metric `json:"contextFiles,omitempty"`

	Overall        Metric `json:"overall"`
	DurationMicros int64  `json:"durationMicros"`
	SampleCount    int    `json:"sampleCount"`
	ProfileCount   int    `json:"profileCount"`

	// TimingApproximate is set when self-times were derived from hitCounts
	// (legacy profiles without samples/timeDeltas) rather than timeDeltas.
	TimingApproximate bool `json:"timingApproximate"`
}

// nodeSelf is the per-node self cost computed before aggregation.
type nodeSelf struct {
	samples int64
	micros  int64
}

// AggregateProfile rolls a single parsed profile up into function/file/package
// entities with self and recursion-collapsed total metrics.
func AggregateProfile(p *Profile) *Aggregation {
	idToNode := make(map[int]*Node, len(p.Nodes))
	for i := range p.Nodes {
		idToNode[p.Nodes[i].ID] = &p.Nodes[i]
	}

	self := make(map[int]*nodeSelf, len(p.Nodes))
	for id := range idToNode {
		self[id] = &nodeSelf{}
	}

	// Context attribution (optional): per-context overall self, and per-node
	// per-context self so the tree walk can roll up function->context inclusive.
	hasCtx := p.Async != nil && len(p.Async.Samples) == len(p.Samples)
	ctxSelf := map[string]*nodeSelf{}             // label -> overall self
	nodeCtxSelf := map[int]map[string]*nodeSelf{} // nodeID -> label -> self
	addCtx := func(id, i int) {
		li := p.Async.Samples[i]
		if li < 0 || li >= len(p.Async.Labels) {
			return // unattributed sample
		}
		label := p.Async.Labels[li]
		cs := ctxSelf[label]
		if cs == nil {
			cs = &nodeSelf{}
			ctxSelf[label] = cs
		}
		cs.samples++
		cs.micros += p.TimeDeltas[i]
		m := nodeCtxSelf[id]
		if m == nil {
			m = map[string]*nodeSelf{}
			nodeCtxSelf[id] = m
		}
		ns := m[label]
		if ns == nil {
			ns = &nodeSelf{}
			m[label] = ns
		}
		ns.samples++
		ns.micros += p.TimeDeltas[i]
	}

	approximate := false
	if len(p.Samples) > 0 {
		for i, id := range p.Samples {
			s, ok := self[id]
			if !ok {
				continue // sample references an unknown/truncated node
			}
			s.samples++
			s.micros += p.TimeDeltas[i]
			if hasCtx {
				addCtx(id, i)
			}
		}
	} else {
		// Fallback: use hitCount, distributing wall duration proportionally.
		approximate = true
		var totalHits int64
		for i := range p.Nodes {
			totalHits += int64(p.Nodes[i].HitCount)
		}
		duration := p.EndTime - p.StartTime
		for i := range p.Nodes {
			n := &p.Nodes[i]
			s := self[n.ID]
			s.samples = int64(n.HitCount)
			if totalHits > 0 && duration > 0 {
				s.micros = int64(n.HitCount) * duration / totalHits
			}
		}
	}

	agg := &Aggregation{
		Functions:         make(map[string]*Entity),
		Files:             make(map[string]*Entity),
		Packages:          make(map[string]*Entity),
		ProfileCount:      1,
		SampleCount:       len(p.Samples),
		TimingApproximate: approximate,
	}
	if agg.SampleCount == 0 {
		// Legacy profile: treat total hits as the sample count.
		for id := range self {
			agg.SampleCount += int(self[id].samples)
		}
	}
	if d := p.EndTime - p.StartTime; d > 0 {
		agg.DurationMicros = d
	}

	// Overall = sum of all self.
	for id := range self {
		agg.Overall.SelfSamples += self[id].samples
		agg.Overall.SelfMicros += self[id].micros
	}
	agg.Overall.TotalSamples = agg.Overall.SelfSamples
	agg.Overall.TotalMicros = agg.Overall.SelfMicros

	// Per-context overall entities (self == total at this level).
	if hasCtx {
		agg.Contexts = make(map[string]*Entity, len(ctxSelf))
		for label, cs := range ctxSelf {
			agg.Contexts[label] = &Entity{
				Key: label, Display: label, Kind: KindContext,
				Metric: Metric{
					SelfSamples: cs.samples, TotalSamples: cs.samples,
					SelfMicros: cs.micros, TotalMicros: cs.micros,
				},
			}
		}
		agg.FunctionContexts = make(map[string]map[string]Metric)
		agg.ContextPackages = make(map[string]map[string]Metric)
		agg.ContextFiles = make(map[string]map[string]Metric)
	}

	roots := findRoots(p.Nodes)
	aggregateTree(idToNode, self, roots, agg, nodeCtxSelf)
	agg.Edges = buildEdges(idToNode, self, roots)

	return agg
}

// buildEdges constructs the function call graph: for each parent→child node
// edge, it attributes the child node's subtree inclusive cost to the
// caller→callee function pair. Edges are node-level, so a recursive function
// simply yields a self-edge — no path-dedup is needed here (unlike the inclusive
// totals in aggregateTree). Returns nil when there are no edges.
func buildEdges(idToNode map[int]*Node, self map[int]*nodeSelf, roots []int) map[string]map[string]Metric {
	if len(idToNode) == 0 {
		return nil
	}
	// Subtree inclusive cost per node (self of the whole subtree).
	totals := subtreeTotals(idToNode, self, roots)

	// Cache each node's function key (keysFor is not free).
	fnKey := make(map[int]string, len(idToNode))
	keyOf := func(id int) string {
		k, ok := fnKey[id]
		if !ok {
			k = keysFor(idToNode[id].CallFrame).fnKey
			fnKey[id] = k
		}
		return k
	}

	edges := make(map[string]map[string]Metric)
	for id, n := range idToNode {
		if len(n.Children) == 0 {
			continue
		}
		pk := keyOf(id)
		for _, c := range n.Children {
			if idToNode[c] == nil {
				continue
			}
			ck := keyOf(c)
			ct := totals[c]
			row := edges[pk]
			if row == nil {
				row = make(map[string]Metric)
				edges[pk] = row
			}
			m := row[ck]
			m.TotalSamples += ct.samples
			m.TotalMicros += ct.micros
			row[ck] = m
		}
	}
	if len(edges) == 0 {
		return nil
	}
	return edges
}

// subtreeTotals computes each node's inclusive cost (sum of self over its
// subtree) with an iterative post-order walk so deep trees don't overflow.
func subtreeTotals(idToNode map[int]*Node, self map[int]*nodeSelf, roots []int) map[int]nodeSelf {
	totals := make(map[int]nodeSelf, len(idToNode))
	type frame struct {
		id  int
		idx int
	}
	for _, root := range roots {
		stack := []*frame{{id: root}}
		for len(stack) > 0 {
			fr := stack[len(stack)-1]
			n := idToNode[fr.id]
			if n == nil {
				stack = stack[:len(stack)-1]
				continue
			}
			if fr.idx < len(n.Children) {
				c := n.Children[fr.idx]
				fr.idx++
				stack = append(stack, &frame{id: c})
				continue
			}
			var t nodeSelf
			if s := self[fr.id]; s != nil {
				t.samples, t.micros = s.samples, s.micros
			}
			for _, c := range n.Children {
				ct := totals[c]
				t.samples += ct.samples
				t.micros += ct.micros
			}
			totals[fr.id] = t
			stack = stack[:len(stack)-1]
		}
	}
	return totals
}

// findRoots returns node ids that are never referenced as a child.
func findRoots(nodes []Node) []int {
	isChild := make(map[int]bool, len(nodes))
	for i := range nodes {
		for _, c := range nodes[i].Children {
			isChild[c] = true
		}
	}
	var roots []int
	for i := range nodes {
		if !isChild[nodes[i].ID] {
			roots = append(roots, nodes[i].ID)
		}
	}
	return roots
}

// entityKeys derives the function/file/package keys (and display labels) for a
// call frame.
type entityKeys struct {
	fnKey, fnDisplay string
	fileKey          string
	pkgKey           string
	category         string
}

func keysFor(cf CallFrame) entityKeys {
	kind, pkg := DerivePackage(cf.URL, cf.FunctionName)
	cat := Category(kind, pkg)

	fnName := cf.FunctionName
	if fnName == "" {
		fnName = "(anonymous)"
	}
	line := strconv.Itoa(cf.LineNumber + 1)

	var fnKey string
	if cf.ScriptID != "" {
		fnKey = cf.ScriptID + ":" + line + ":" + cf.FunctionName
	} else {
		fnKey = cf.URL + ":" + line + ":" + cf.FunctionName
	}

	fileKey := cf.URL
	if fileKey == "" {
		fileKey = "(native)"
	}

	fnDisplay := fnName
	if cf.URL != "" {
		fnDisplay = fnName + " (" + cf.URL + ":" + line + ")"
	}

	return entityKeys{
		fnKey:     fnKey,
		fnDisplay: fnDisplay,
		fileKey:   fileKey,
		pkgKey:    pkg,
		category:  cat,
	}
}

// aggregateTree walks the call tree depth-first, accumulating self into each
// entity and inclusive totals while collapsing recursion: a node's self is
// added to an entity's Total at most once per node even if the entity recurs on
// the current path.
func aggregateTree(idToNode map[int]*Node, self map[int]*nodeSelf, roots []int, agg *Aggregation, nodeCtxSelf map[int]map[string]*nodeSelf) {
	// Reference counts of keys currently active on the DFS path.
	fnActive := map[string]int{}
	fileActive := map[string]int{}
	pkgActive := map[string]int{}

	type frame struct {
		id        int
		childIdx  int
		k         entityKeys
		fnFirst   bool // this node was the first to activate its fn key
		fileFirst bool
		pkgFirst  bool
		entered   bool
	}

	getEntity := func(m map[string]*Entity, key, display, pkg, cat string, kind EntityKind) *Entity {
		e := m[key]
		if e == nil {
			e = &Entity{Key: key, Display: display, Kind: kind, Package: pkg, Category: cat}
			m[key] = e
		}
		return e
	}

	for _, root := range roots {
		stack := []*frame{{id: root}}
		for len(stack) > 0 {
			fr := stack[len(stack)-1]
			node := idToNode[fr.id]
			if node == nil {
				stack = stack[:len(stack)-1]
				continue
			}

			if !fr.entered {
				fr.entered = true
				fr.k = keysFor(node.CallFrame)
				s := self[fr.id]

				fnE := getEntity(agg.Functions, fr.k.fnKey, fr.k.fnDisplay, fr.k.pkgKey, fr.k.category, KindFunction)
				fnE.File = fr.k.fileKey // owning file, for exact file->function membership
				fileE := getEntity(agg.Files, fr.k.fileKey, fr.k.fileKey, fr.k.pkgKey, fr.k.category, KindFile)
				pkgE := getEntity(agg.Packages, fr.k.pkgKey, fr.k.pkgKey, fr.k.pkgKey, fr.k.category, KindPackage)

				// Self is always additive.
				fnE.Metric.SelfSamples += s.samples
				fnE.Metric.SelfMicros += s.micros
				fileE.Metric.SelfSamples += s.samples
				fileE.Metric.SelfMicros += s.micros
				pkgE.Metric.SelfSamples += s.samples
				pkgE.Metric.SelfMicros += s.micros

				// Total: add this node's self to every distinct active key once.
				// Activate keys for this node; record which we activated so we
				// can deactivate on exit.
				fr.fnFirst = fnActive[fr.k.fnKey] == 0
				fnActive[fr.k.fnKey]++
				fr.fileFirst = fileActive[fr.k.fileKey] == 0
				fileActive[fr.k.fileKey]++
				fr.pkgFirst = pkgActive[fr.k.pkgKey] == 0
				pkgActive[fr.k.pkgKey]++

				// This node's self contributes to the inclusive total of every
				// entity active on the path. Because each entity is counted via
				// its (deduped) active set, recursion does not double-count: we
				// add s once per distinct active key.
				addSelfToActiveTotals(agg, self[fr.id], fnActive, fileActive, pkgActive)

				// Same dedup logic for the per-context inclusive rollup: split
				// this node's self by label and add to each active function once.
				// The node's own self also belongs to its leaf frame's package and
				// file, so attribute it there per label (self-only, no recursion
				// concern: self is always additive).
				if agg.FunctionContexts != nil {
					if ncs := nodeCtxSelf[fr.id]; len(ncs) > 0 {
						addNodeContextsToActiveFns(agg, ncs, fnActive)
						for label, ns := range ncs {
							addCtxEntitySelf(agg.ContextPackages, label, fr.k.pkgKey, ns)
							addCtxEntitySelf(agg.ContextFiles, label, fr.k.fileKey, ns)
						}
					}
				}
			}

			if fr.childIdx < len(node.Children) {
				childID := node.Children[fr.childIdx]
				fr.childIdx++
				stack = append(stack, &frame{id: childID})
				continue
			}

			// Exit: deactivate keys this node activated.
			if fr.fnFirst {
				delete(fnActive, fr.k.fnKey)
			} else {
				fnActive[fr.k.fnKey]--
			}
			if fr.fileFirst {
				delete(fileActive, fr.k.fileKey)
			} else {
				fileActive[fr.k.fileKey]--
			}
			if fr.pkgFirst {
				delete(pkgActive, fr.k.pkgKey)
			} else {
				pkgActive[fr.k.pkgKey]--
			}
			stack = stack[:len(stack)-1]
		}
	}
}

// addSelfToActiveTotals adds a node's self cost to the Total of every entity key
// currently active on the path, exactly once per distinct key.
func addSelfToActiveTotals(agg *Aggregation, s *nodeSelf, fnActive, fileActive, pkgActive map[string]int) {
	for key := range fnActive {
		if e := agg.Functions[key]; e != nil {
			e.Metric.TotalSamples += s.samples
			e.Metric.TotalMicros += s.micros
		}
	}
	for key := range fileActive {
		if e := agg.Files[key]; e != nil {
			e.Metric.TotalSamples += s.samples
			e.Metric.TotalMicros += s.micros
		}
	}
	for key := range pkgActive {
		if e := agg.Packages[key]; e != nil {
			e.Metric.TotalSamples += s.samples
			e.Metric.TotalMicros += s.micros
		}
	}
}

// addCtxEntitySelf adds a node's per-context self (for one label) to the package
// or file map keyed by that label, as both Self and Total (Self == Total at this
// aggregated level, like Contexts). m must be non-nil.
func addCtxEntitySelf(m map[string]map[string]Metric, label, key string, ns *nodeSelf) {
	row := m[label]
	if row == nil {
		row = make(map[string]Metric)
		m[label] = row
	}
	cur := row[key]
	cur.SelfSamples += ns.samples
	cur.SelfMicros += ns.micros
	cur.TotalSamples += ns.samples
	cur.TotalMicros += ns.micros
	row[key] = cur
}

// addNodeContextsToActiveFns rolls a node's per-context self into the inclusive
// FunctionContexts of every function active on the path, once per distinct
// function (mirroring the recursion-collapse in addSelfToActiveTotals).
func addNodeContextsToActiveFns(agg *Aggregation, ncs map[string]*nodeSelf, fnActive map[string]int) {
	for fnKey := range fnActive {
		row := agg.FunctionContexts[fnKey]
		if row == nil {
			row = make(map[string]Metric)
			agg.FunctionContexts[fnKey] = row
		}
		for label, ns := range ncs {
			m := row[label]
			m.TotalSamples += ns.samples
			m.TotalMicros += ns.micros
			row[label] = m
		}
	}
}
