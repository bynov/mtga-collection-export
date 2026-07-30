// Package oracle validates candidate collection tables against facts we trust
// and reconciles the survivors. It never returns an unvalidated result.
package oracle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bynov/mtga-collection-export/internal/scan"
)

// Oracle holds the ground-truth facts used to judge candidates.
type Oracle struct {
	Valid          map[uint32]bool
	NonCollectible map[uint32]bool
	Basics         map[uint32]bool
	KnownOwned     map[uint32]uint32 // grpId -> copies proven by played decks
	KnownAbsent    map[uint32]bool
}

// Result is a validated collection.
type Result struct {
	Counts   map[uint32]uint32
	Variants int
	Degraded bool
	Evidence string
}

type verdict struct {
	cand                    scan.Candidate
	missing, forbidden      int
	nonCollectible, overCap int
	qtyViolations           int
}

func (o *Oracle) judge(c scan.Candidate) verdict {
	v := verdict{cand: c}
	for g := range o.KnownOwned {
		if _, ok := c.Entries[g]; !ok {
			v.missing++
		}
	}
	for g, cnt := range c.Entries {
		if o.KnownAbsent[g] {
			v.forbidden++
		}
		if o.NonCollectible[g] {
			v.nonCollectible++
		}
		if cnt > 4 && !o.Basics[g] {
			v.overCap++
		}
		if want, ok := o.KnownOwned[g]; ok && cnt < want && !o.Basics[g] {
			v.qtyViolations++
		}
	}
	return v
}

func (v verdict) passes() bool {
	return v.missing == 0 && v.forbidden == 0 && v.nonCollectible == 0 && v.overCap == 0 && v.qtyViolations == 0
}

// Evaluate selects passing candidates and returns their element-wise max.
// Ownership is monotonic, so a lower count is a stale/working copy (handoff §7.8).
func (o *Oracle) Evaluate(cands []scan.Candidate) (*Result, error) {
	degraded := len(o.KnownOwned) == 0
	var passing []scan.Candidate
	var rejects []verdict
	for _, c := range cands {
		v := o.judge(c)
		if v.passes() {
			passing = append(passing, c)
		} else {
			rejects = append(rejects, v)
		}
	}
	if len(passing) == 0 {
		return nil, fmt.Errorf("no candidate passed validation\n%s", evidence(rejects))
	}
	counts := map[uint32]uint32{}
	for _, c := range passing {
		for g, n := range c.Entries {
			if n > counts[g] {
				counts[g] = n
			}
		}
	}
	return &Result{Counts: counts, Variants: len(passing), Degraded: degraded, Evidence: passSummary(passing)}, nil
}

func passSummary(passing []scan.Candidate) string {
	sizes := make([]int, len(passing))
	for i, c := range passing {
		sizes[i] = len(c.Entries)
	}
	sort.Ints(sizes)
	return fmt.Sprintf("%d passing variants, entry counts %v", len(passing), sizes)
}

func evidence(rejects []verdict) string {
	sort.Slice(rejects, func(i, j int) bool { return len(rejects[i].cand.Entries) > len(rejects[j].cand.Entries) })
	var b strings.Builder
	fmt.Fprintf(&b, "%6s %6s %8s %8s %8s %8s %8s\n", "stride", "cnt@", "cards", "missing", "absent", "noncol", ">4")
	for i, v := range rejects {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "%6d %+6d %8d %8d %8d %8d %8d\n",
			v.cand.Stride, v.cand.CountOffset, len(v.cand.Entries), v.missing, v.forbidden, v.nonCollectible, v.overCap)
	}
	return b.String()
}
