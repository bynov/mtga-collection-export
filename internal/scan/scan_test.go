package scan

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/machocore"
	"github.com/bynov/mtga-collection-export/internal/testcore"
)

func TestFindCandidatesRecoversPlantedCollection(t *testing.T) {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	b, err := testcore.Build(t.TempDir(), testcore.Spec{
		Valid:  valid,
		Owned:  map[uint32]uint32{7000: 4, 7010: 2, 7020: 1},
		Absent: []uint32{9999999},
		Seed:   1337,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := machocore.Open(b.CorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	lut := map[uint32]bool{}
	for _, id := range valid {
		lut[id] = true
	}
	cands := FindCandidates(c, lut, 4)
	if len(cands) == 0 {
		t.Fatal("no candidates found")
	}

	// At least one candidate must reproduce the planted collection exactly.
	// Because the dict's hash field equals the key (grpId < 2^31), keys and
	// hashes interleave on an 8-byte grid, so the collection reads as a packed
	// stride-8 array with the count at +4 — exactly the real-world shape the
	// handoff observed (§5: "stride=8 count@+4").
	var matched *Candidate
	for i := range cands {
		if len(cands[i].Entries) != len(b.Truth) {
			continue
		}
		ok := true
		for g, n := range b.Truth {
			if cands[i].Entries[g] != n {
				ok = false
				break
			}
		}
		if ok {
			matched = &cands[i]
			break
		}
	}
	if matched == nil {
		t.Fatal("no candidate reproduced the planted collection exactly")
	}
	if matched.Stride != 8 || matched.CountOffset != 4 {
		t.Fatalf("recovered shape stride=%d count@%+d, want stride=8 count@+4", matched.Stride, matched.CountOffset)
	}
}
