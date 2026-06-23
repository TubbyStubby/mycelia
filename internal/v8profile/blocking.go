package v8profile

import "sort"

// DefaultBlockThresholdMicros is the long-task threshold used by the
// AggregateProfile convenience wrapper. 50ms matches the browser "Long Task"
// convention: an uninterrupted span during which the event loop could not do
// other work.
const DefaultBlockThresholdMicros = 50_000

// maxStallStackDepth caps a captured stall stack (root->leaf) so a deep call
// tree does not bloat the aggregation. Leaf-side frames are kept when deeper.
const maxStallStackDepth = 32

// topStallsLimit bounds how many worst-case stall episodes an aggregation keeps.
const topStallsLimit = 64

// BlockStat is one entity's contribution to event-loop blocking: self time
// accrued inside long-task episodes, the number of episodes it dominated (was
// the most-self leaf of), and the largest single episode it dominated.
type BlockStat struct {
	BlockedMicros    int64 `json:"blockedMicros"`
	Episodes         int64 `json:"episodes"`
	MaxEpisodeMicros int64 `json:"maxEpisodeMicros"`
}

func (s *BlockStat) add(o BlockStat) {
	s.BlockedMicros += o.BlockedMicros
	s.Episodes += o.Episodes
	s.bumpMax(o.MaxEpisodeMicros)
}

func (s *BlockStat) bumpMax(v int64) {
	if v > s.MaxEpisodeMicros {
		s.MaxEpisodeMicros = v
	}
}

// Stall is one long-task episode: a contiguous run of non-idle samples whose
// wall-time reached the threshold, captured with the call stack of its
// largest-timeDelta sample (the moment most likely to be the blocking call).
type Stall struct {
	DurationMicros int64    `json:"durationMicros"`
	Samples        int      `json:"samples"`
	Context        string   `json:"context,omitempty"` // dominant async label (route/job)
	LeafDisplay    string   `json:"leafDisplay"`
	Stack          []string `json:"stack,omitempty"` // root->leaf display frames, capped
}

// Blocking is the event-loop blocking rollup for an aggregation: episodes that
// held the loop for >= ThresholdMicros, attributed to leaf functions and async
// contexts, plus the worst individual stalls with their stacks. It is nil when
// the profile carried no per-sample timing (legacy) or had no long tasks.
type Blocking struct {
	ThresholdMicros  int64                 `json:"thresholdMicros"`
	Episodes         int64                 `json:"episodes"`
	BlockedMicros    int64                 `json:"blockedMicros"`
	MaxEpisodeMicros int64                 `json:"maxEpisodeMicros"`
	Functions        map[string]*BlockStat `json:"functions,omitempty"`
	Contexts         map[string]*BlockStat `json:"contexts,omitempty"`
	TopStalls        []Stall               `json:"topStalls,omitempty"`
}

// episode accumulates one in-progress run of consecutive non-idle samples.
type episode struct {
	micros   int64
	samples  int
	leafMic  map[string]int64 // leaf fn key -> self micros within this episode
	domKey   string           // leaf fn key with the most micros so far
	domMic   int64
	ctxMic   map[string]int64 // async label -> micros within this episode
	maxDelta int64            // largest single timeDelta seen
	maxIdx   int              // sample index of that largest timeDelta
}

// detectBlocking scans the sample stream for long-task episodes — maximal runs
// of non-idle samples whose summed timeDeltas reach thresholdMicros — and
// attributes each to its leaf function(s), dominant async context, and the call
// stack of its largest sample. Idle samples (CatIdle) are episode boundaries,
// matching the busy/idle split used elsewhere. Returns nil when there is no
// per-sample timing or no episode crossed the threshold.
func detectBlocking(p *Profile, idToNode map[int]*Node, thresholdMicros int64) *Blocking {
	if len(p.Samples) == 0 || thresholdMicros <= 0 {
		return nil
	}
	hasCtx := p.Async != nil && len(p.Async.Samples) == len(p.Samples)

	parent := make(map[int]int, len(idToNode))
	for id, n := range idToNode {
		for _, c := range n.Children {
			parent[c] = id
		}
	}

	keyCache := make(map[int]entityKeys, len(idToNode))
	keyOf := func(id int) (entityKeys, bool) {
		n := idToNode[id]
		if n == nil {
			return entityKeys{}, false
		}
		k, ok := keyCache[id]
		if !ok {
			k = keysFor(n.CallFrame)
			keyCache[id] = k
		}
		return k, true
	}

	b := &Blocking{ThresholdMicros: thresholdMicros}
	var ep *episode

	closeEp := func() {
		if ep != nil && ep.micros >= thresholdMicros {
			b.record(ep, p, parent, keyOf)
		}
		ep = nil
	}

	for i, id := range p.Samples {
		k, ok := keyOf(id)
		if !ok || k.category == CatIdle {
			closeEp()
			continue
		}
		if ep == nil {
			ep = &episode{maxIdx: i, maxDelta: -1}
		}
		d := p.TimeDeltas[i]
		ep.micros += d
		ep.samples++
		if ep.leafMic == nil {
			ep.leafMic = map[string]int64{}
		}
		ep.leafMic[k.fnKey] += d
		if ep.leafMic[k.fnKey] > ep.domMic {
			ep.domMic = ep.leafMic[k.fnKey]
			ep.domKey = k.fnKey
		}
		if hasCtx {
			if lbl := contextLabel(p, i); lbl != "" {
				if ep.ctxMic == nil {
					ep.ctxMic = map[string]int64{}
				}
				ep.ctxMic[lbl] += d
			}
		}
		if d > ep.maxDelta {
			ep.maxDelta = d
			ep.maxIdx = i
		}
	}
	closeEp()

	if b.Episodes == 0 {
		return nil
	}
	return b
}

// record folds one above-threshold episode into the Blocking rollup.
func (b *Blocking) record(ep *episode, p *Profile, parent map[int]int, keyOf func(int) (entityKeys, bool)) {
	b.Episodes++
	b.BlockedMicros += ep.micros
	if ep.micros > b.MaxEpisodeMicros {
		b.MaxEpisodeMicros = ep.micros
	}

	if b.Functions == nil {
		b.Functions = map[string]*BlockStat{}
	}
	for fnKey, mic := range ep.leafMic {
		st := stat(b.Functions, fnKey)
		st.BlockedMicros += mic
		if fnKey == ep.domKey {
			st.Episodes++
			st.bumpMax(ep.micros)
		}
	}

	domLbl := dominantKey(ep.ctxMic)
	if len(ep.ctxMic) > 0 {
		if b.Contexts == nil {
			b.Contexts = map[string]*BlockStat{}
		}
		for lbl, mic := range ep.ctxMic {
			stat(b.Contexts, lbl).BlockedMicros += mic
		}
		dom := stat(b.Contexts, domLbl)
		dom.Episodes++
		dom.bumpMax(ep.micros)
	}

	leafID := p.Samples[ep.maxIdx]
	s := Stall{
		DurationMicros: ep.micros,
		Samples:        ep.samples,
		Context:        domLbl,
		Stack:          buildStack(leafID, parent, keyOf),
	}
	if k, ok := keyOf(leafID); ok {
		s.LeafDisplay = k.fnDisplay
	}
	b.TopStalls = insertStall(b.TopStalls, s)
}

// mergeBlocking folds src into *dst, allocating *dst lazily so a group with no
// blocking data stays nil. Stats sum; max-episode takes the max; top stalls
// merge into one descending, capped list (so a group's stalls are the globally
// worst episodes across its profiles).
func mergeBlocking(dst **Blocking, src *Blocking) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &Blocking{ThresholdMicros: src.ThresholdMicros}
	}
	d := *dst
	if d.ThresholdMicros == 0 {
		d.ThresholdMicros = src.ThresholdMicros
	}
	d.Episodes += src.Episodes
	d.BlockedMicros += src.BlockedMicros
	if src.MaxEpisodeMicros > d.MaxEpisodeMicros {
		d.MaxEpisodeMicros = src.MaxEpisodeMicros
	}
	mergeBlockStats(&d.Functions, src.Functions)
	mergeBlockStats(&d.Contexts, src.Contexts)
	for _, s := range src.TopStalls {
		d.TopStalls = insertStall(d.TopStalls, s)
	}
}

func mergeBlockStats(dst *map[string]*BlockStat, src map[string]*BlockStat) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]*BlockStat, len(src))
	}
	for k, st := range src {
		stat(*dst, k).add(*st)
	}
}

func stat(m map[string]*BlockStat, key string) *BlockStat {
	st := m[key]
	if st == nil {
		st = &BlockStat{}
		m[key] = st
	}
	return st
}

// dominantKey returns the map key with the largest value, or "" when empty.
func dominantKey(m map[string]int64) string {
	var best string
	var bestV int64
	for k, v := range m {
		if v > bestV {
			bestV, best = v, k
		}
	}
	return best
}

// contextLabel returns the async label for sample i, or "" when unattributed.
func contextLabel(p *Profile, i int) string {
	li := p.Async.Samples[i]
	if li < 0 || li >= len(p.Async.Labels) {
		return ""
	}
	return p.Async.Labels[li]
}

// buildStack returns the call stack of leafID as root->leaf display strings,
// keeping at most maxStallStackDepth innermost frames. A seen-set guards against
// a malformed parent cycle.
func buildStack(leafID int, parent map[int]int, keyOf func(int) (entityKeys, bool)) []string {
	var rev []string // leaf->root
	seen := map[int]bool{}
	for id := leafID; !seen[id]; {
		seen[id] = true
		k, ok := keyOf(id)
		if !ok {
			break
		}
		rev = append(rev, k.fnDisplay)
		pid, ok := parent[id]
		if !ok {
			break
		}
		id = pid
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if len(rev) > maxStallStackDepth {
		rev = rev[len(rev)-maxStallStackDepth:]
	}
	return rev
}

// insertStall inserts s into a slice kept sorted by descending duration, capped
// at topStallsLimit entries.
func insertStall(stalls []Stall, s Stall) []Stall {
	if len(stalls) >= topStallsLimit && s.DurationMicros <= stalls[len(stalls)-1].DurationMicros {
		return stalls
	}
	idx := sort.Search(len(stalls), func(i int) bool {
		return stalls[i].DurationMicros < s.DurationMicros
	})
	stalls = append(stalls, Stall{})
	copy(stalls[idx+1:], stalls[idx:])
	stalls[idx] = s
	if len(stalls) > topStallsLimit {
		stalls = stalls[:topStallsLimit]
	}
	return stalls
}
