package testcore

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/machocore"
)

func smallSpec() Spec {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	return Spec{
		Valid:  valid,
		Owned:  map[uint32]uint32{7000: 4, 7010: 2, 7020: 1},
		Absent: []uint32{9999999, 9999998}, // not in Valid, planted only in the decoy
		Seed:   1337,
	}
}

func TestBuildIsParseableAndPlantsTruth(t *testing.T) {
	b, err := Build(t.TempDir(), smallSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Truth) == 0 {
		t.Fatal("no planted truth")
	}
	for _, g := range []uint32{7000, 7010, 7020} {
		if _, ok := b.Truth[g]; !ok {
			t.Fatalf("owned card %d missing from planted truth", g)
		}
	}
	c, err := machocore.Open(b.CorePath)
	if err != nil {
		t.Fatalf("built core not parseable: %v", err)
	}
	defer c.Close()
	if len(c.Segments()) < 5 {
		t.Fatalf("expected >=5 segments (collection + 4 decoys), got %d", len(c.Segments()))
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, _ := Build(t.TempDir(), smallSpec())
	b, _ := Build(t.TempDir(), smallSpec())
	if len(a.Truth) != len(b.Truth) {
		t.Fatalf("nondeterministic truth size: %d vs %d", len(a.Truth), len(b.Truth))
	}
	for k, v := range a.Truth {
		if b.Truth[k] != v {
			t.Fatalf("nondeterministic truth for %d: %d vs %d", k, v, b.Truth[k])
		}
	}
}
