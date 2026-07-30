// Package scan locates candidate collection tables in a core dump without
// assuming their layout. The oracle layer validates the candidates it returns.
package scan

import (
	"encoding/binary"
	"sort"

	"github.com/bynov/mtga-collection-export/internal/machocore"
)

var strides = []int{8, 12, 16, 20, 24, 28, 32, 40, 48, 64}
var countOffsets = []int{-16, -12, -8, -4, 4, 8, 12, 16}

const (
	maxGap  = 512
	minHits = 60
)

// Candidate is one hypothesised table: a run at (Base, Stride) with the count
// field at Base+CountOffset, and the grpId->count map it yields.
type Candidate struct {
	Base        uint64
	Stride      int
	CountOffset int
	Entries     map[uint32]uint32
}

// collectHits maps every valid-grpId sighting address to its grpId.
func collectHits(c *machocore.Core, valid map[uint32]bool, align int) map[uint64]uint32 {
	hits := map[uint64]uint32{}
	shifts := []int{0}
	if align < 4 {
		shifts = []int{0, 1, 2, 3}
		align = 1
	}
	for _, s := range c.Segments() {
		buf, err := c.SegmentBytes(s)
		if err != nil {
			continue
		}
		for _, sh := range shifts {
			for off := sh; off+4 <= len(buf); off += align {
				v := binary.LittleEndian.Uint32(buf[off:])
				if valid[v] {
					hits[s.VMAddr+uint64(off)] = v
				}
			}
		}
	}
	return hits
}

// clusters splits sorted sighting addresses into byte-proximity groups.
func clusters(addrs []uint64) [][]uint64 {
	if len(addrs) == 0 {
		return nil
	}
	var out [][]uint64
	cur := []uint64{addrs[0]}
	for _, a := range addrs[1:] {
		if a-cur[len(cur)-1] <= maxGap {
			cur = append(cur, a)
		} else {
			if len(cur) >= minHits {
				out = append(out, cur)
			}
			cur = []uint64{a}
		}
	}
	if len(cur) >= minHits {
		out = append(out, cur)
	}
	return out
}

// residueRuns groups a cluster's addresses by addr % stride.
func residueRuns(cluster []uint64, stride int) [][]uint64 {
	byRes := map[uint64][]uint64{}
	for _, a := range cluster {
		byRes[a%uint64(stride)] = append(byRes[a%uint64(stride)], a)
	}
	var out [][]uint64
	for _, g := range byRes {
		if len(g) >= minHits {
			sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
			out = append(out, g)
		}
	}
	return out
}

// FindCandidates enumerates every (stride, count-offset) hypothesis with enough
// entries. align=4 scans the word grid; align<4 falls back to unaligned.
func FindCandidates(c *machocore.Core, valid map[uint32]bool, align int) []Candidate {
	hits := collectHits(c, valid, align)
	if len(hits) == 0 {
		return nil
	}
	addrs := make([]uint64, 0, len(hits))
	for a := range hits {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })

	var out []Candidate
	for _, cl := range clusters(addrs) {
		for _, stride := range strides {
			for _, run := range residueRuns(cl, stride) {
				for _, delta := range countOffsets {
					entries := make(map[uint32]uint32, len(run))
					for _, a := range run {
						cnt, ok := c.ReadU32(uint64(int64(a) + int64(delta)))
						if !ok {
							continue
						}
						if cnt >= 1 && cnt <= 64 {
							entries[hits[a]] = cnt
						}
					}
					if len(entries) >= minHits {
						out = append(out, Candidate{Base: run[0], Stride: stride, CountOffset: delta, Entries: entries})
					}
				}
			}
		}
	}
	return out
}
