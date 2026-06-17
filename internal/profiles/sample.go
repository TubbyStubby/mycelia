package profiles

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// Sample deterministically selects up to n members from a group. Selection is
// pseudo-random but stable for a given member set (members are ranked by a hash
// of their object name), so results are reproducible and group caches stay
// warm. n <= 0 or n >= len(members) returns all members unchanged.
//
// The returned slice is a new copy; the input is not modified.
func Sample(members []GroupMember, n int) []GroupMember {
	if n <= 0 || n >= len(members) {
		out := make([]GroupMember, len(members))
		copy(out, members)
		return out
	}

	ranked := make([]GroupMember, len(members))
	copy(ranked, members)
	sort.Slice(ranked, func(i, j int) bool {
		hi, hj := memberHash(ranked[i]), memberHash(ranked[j])
		if hi != hj {
			return hi < hj
		}
		return ranked[i].Key.Raw < ranked[j].Key.Raw
	})
	return ranked[:n]
}

func memberHash(m GroupMember) uint64 {
	sum := sha256.Sum256([]byte(m.Key.Raw))
	return binary.BigEndian.Uint64(sum[:8])
}
