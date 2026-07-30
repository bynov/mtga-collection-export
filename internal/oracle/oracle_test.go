package oracle

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/scan"
)

func base() *Oracle {
	return &Oracle{
		Valid:          map[uint32]bool{7000: true, 7010: true, 7020: true, 8000: true},
		NonCollectible: map[uint32]bool{999: true},
		Basics:         map[uint32]bool{7020: true},
		KnownOwned:     map[uint32]uint32{7000: 4, 7010: 2},
		KnownAbsent:    map[uint32]bool{9999999: true},
	}
}

func TestEvaluateSelectsAndReconcilesMax(t *testing.T) {
	// Two variants that both pass structural tests but disagree on 8000 (a valid
	// card not in the oracle — a stale working copy under-reports it). The
	// element-wise max must win (handoff §7.8: ownership is monotonic).
	v0 := scan.Candidate{Stride: 8, CountOffset: 4, Entries: map[uint32]uint32{7000: 4, 7010: 2, 8000: 3}}
	v1 := scan.Candidate{Stride: 8, CountOffset: 4, Entries: map[uint32]uint32{7000: 4, 7010: 2, 8000: 2}}
	res, err := base().Evaluate([]scan.Candidate{v1, v0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[8000] != 3 {
		t.Fatalf("reconcile should take max: 8000 = %d, want 3", res.Counts[8000])
	}
	if res.Variants != 2 {
		t.Fatalf("Variants = %d, want 2", res.Variants)
	}
}

func TestEvaluateRejectsForbiddenAndOverCap(t *testing.T) {
	o := base()
	bad1 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 9999999: 2}} // known-absent
	bad2 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 8000: 7}}    // non-basic over 4
	bad3 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 999: 1}}     // non-collectible
	bad4 := scan.Candidate{Entries: map[uint32]uint32{7000: 4}}                      // missing proven 7010
	if _, err := o.Evaluate([]scan.Candidate{bad1, bad2, bad3, bad4}); err == nil {
		t.Fatal("expected rejection of all-bad candidate set")
	}
}

func TestBasicLandBelowPlayedCountIsAllowed(t *testing.T) {
	o := base()
	o.KnownOwned[7020] = 24 // a deck ran 24 of a basic land
	good := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 7020: 1}}
	if _, err := o.Evaluate([]scan.Candidate{good}); err != nil {
		t.Fatalf("basic land below played count should pass: %v", err)
	}
}

func TestDegradedWhenNoKnownOwned(t *testing.T) {
	o := base()
	o.KnownOwned = map[uint32]uint32{}
	good := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2}}
	res, err := o.Evaluate([]scan.Candidate{good})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded {
		t.Fatal("expected Degraded=true with no played decks")
	}
}
