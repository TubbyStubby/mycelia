package engine

import (
	"context"
	"sort"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// BlockingView is the shaped event-loop blocking result for one group: the
// headline episode counts plus the top blocking functions, contexts (routes/
// APIs), and the worst individual stalls with their stacks. Per-profile figures
// average over the merged profiles so groups of different sizes compare fairly;
// stalls are absolute worst-cases and are not averaged.
type BlockingView struct {
	ThresholdMicros         int64         `json:"thresholdMicros"`
	Episodes                int64         `json:"episodes"`
	BlockedMicros           int64         `json:"blockedMicros"`
	MaxEpisodeMicros        int64         `json:"maxEpisodeMicros"`
	ProfileCount            int           `json:"profileCount"`
	TotalProfiles           int           `json:"totalProfiles"`
	EpisodesPerProfile      float64       `json:"episodesPerProfile"`
	BlockedMicrosPerProfile float64       `json:"blockedMicrosPerProfile"`
	Functions               []BlockingRow `json:"functions"`
	Contexts                []BlockingRow `json:"contexts"`
	Stalls                  []v8profile.Stall `json:"stalls"`
}

// BlockingRow is one ranked blocking entity (a function or an async context).
type BlockingRow struct {
	Key              string `json:"key"`
	Display          string `json:"display"`
	BlockedMicros    int64  `json:"blockedMicros"`
	Episodes         int64  `json:"episodes"`
	MaxEpisodeMicros int64  `json:"maxEpisodeMicros"`
}

// GroupBlocking returns the event-loop blocking view for a group, capping each
// ranked list at topN (0 = no cap).
func (e *Engine) GroupBlocking(ctx context.Context, id profiles.GroupID, topN int) (BlockingView, error) {
	agg, total, err := e.GroupAggregation(ctx, id)
	if err != nil {
		return BlockingView{}, err
	}
	return buildBlockingView(agg, total, topN), nil
}

func buildBlockingView(agg *v8profile.Aggregation, total, topN int) BlockingView {
	v := BlockingView{ProfileCount: agg.ProfileCount, TotalProfiles: total}
	b := agg.Blocking
	if b == nil {
		return v
	}
	pc := agg.ProfileCount
	if pc < 1 {
		pc = 1
	}
	v.ThresholdMicros = b.ThresholdMicros
	v.Episodes = b.Episodes
	v.BlockedMicros = b.BlockedMicros
	v.MaxEpisodeMicros = b.MaxEpisodeMicros
	v.EpisodesPerProfile = float64(b.Episodes) / float64(pc)
	v.BlockedMicrosPerProfile = float64(b.BlockedMicros) / float64(pc)
	v.Functions = topBlockingRows(b.Functions, agg.Functions, topN)
	v.Contexts = topBlockingRows(b.Contexts, nil, topN) // context key == label == display
	v.Stalls = b.TopStalls
	if topN > 0 && len(v.Stalls) > topN {
		v.Stalls = v.Stalls[:topN]
	}
	return v
}

// topBlockingRows ranks block stats by blocked micros (desc), resolving display
// names from the entity map when given (nil -> key is its own display).
func topBlockingRows(stats map[string]*v8profile.BlockStat, display map[string]*v8profile.Entity, topN int) []BlockingRow {
	rows := make([]BlockingRow, 0, len(stats))
	for key, st := range stats {
		disp := key
		if display != nil {
			if e := display[key]; e != nil {
				disp = e.Display
			}
		}
		rows = append(rows, BlockingRow{
			Key:              key,
			Display:          disp,
			BlockedMicros:    st.BlockedMicros,
			Episodes:         st.Episodes,
			MaxEpisodeMicros: st.MaxEpisodeMicros,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].BlockedMicros != rows[j].BlockedMicros {
			return rows[i].BlockedMicros > rows[j].BlockedMicros
		}
		return rows[i].Display < rows[j].Display
	})
	if topN > 0 && len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}
