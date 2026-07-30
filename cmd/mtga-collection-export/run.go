package main

import (
	"fmt"
	"os"

	"github.com/bynov/mtga-collection-export/internal/carddb"
	"github.com/bynov/mtga-collection-export/internal/export"
	"github.com/bynov/mtga-collection-export/internal/machocore"
	"github.com/bynov/mtga-collection-export/internal/oracle"
	"github.com/bynov/mtga-collection-export/internal/scan"
	"github.com/bynov/mtga-collection-export/internal/starthook"
)

// Options configures a run. Core != "" skips acquisition.
type Options struct {
	Core          string
	Out           string
	Format        string
	CardDB        string
	StartHookPath string
	Absent        []uint32
	KeepCore      bool
	Verbose       bool
}

func run(o Options) error {
	db, err := carddb.Open(o.CardDB)
	if err != nil {
		return err
	}
	defer db.Close()

	orc := &oracle.Oracle{
		Valid:          db.Collectible(),
		Basics:         db.Basics(),
		KnownOwned:     map[uint32]uint32{},
		KnownAbsent:    map[uint32]bool{},
		NonCollectible: map[uint32]bool{},
	}
	for _, a := range o.Absent {
		orc.KnownAbsent[a] = true
	}
	if o.StartHookPath != "" {
		raw, err := os.ReadFile(o.StartHookPath)
		if err != nil {
			return err
		}
		sh, err := starthook.Parse(raw)
		if err != nil {
			return err
		}
		orc.KnownOwned = sh.PlayedDeckCards()
		orc.NonCollectible = sh.NonCollectible()
	}

	core, err := machocore.Open(o.Core)
	if err != nil {
		return err
	}
	defer core.Close()

	result, err := validate(core, orc, o.Verbose)
	if err != nil {
		return err // oracle-gated: refuse to emit
	}

	rebalanced := db.Rebalanced()
	var rows []export.Row
	var droppedRebalanced, unresolved int
	for grp, n := range result.Counts {
		if rebalanced[grp] {
			droppedRebalanced++
			continue
		}
		card, ok := db.Resolve(grp)
		if !ok {
			unresolved++
			card = carddb.Card{GrpID: grp}
		}
		rows = append(rows, export.Row{Count: n, Card: card})
	}

	rep, err := export.Write(o.Out, o.Format, rows)
	if err != nil {
		return err
	}

	if result.Degraded {
		fmt.Fprintln(os.Stderr, "WARNING: no played decks found — ran structural tests only, ownership floor not verified.")
	}
	fmt.Printf("VALIDATED (%d variants reconciled by element-wise max)\n", result.Variants)
	fmt.Printf("wrote %s: %d printings, %d distinct names, %d copies\n", o.Out, rep.Printings, rep.DistinctNames, rep.Copies)
	if droppedRebalanced > 0 {
		fmt.Printf("dropped %d Alchemy-rebalanced duplicate rows\n", droppedRebalanced)
	}
	if unresolved > 0 {
		fmt.Printf("note: %d grpIds not in the local card DB (kept as raw ids)\n", unresolved)
	}
	fmt.Println("note: basic lands are unlimited and stored as 1 — a count of 1 does not mean you own only one.")
	return nil
}

// validate scans at align 4, falls back to align 1 if nothing passes. On total
// failure it returns the evidence table from the align-4 pass.
func validate(core *machocore.Core, orc *oracle.Oracle, verbose bool) (*oracle.Result, error) {
	var lastErr error
	for _, align := range []int{4, 1} {
		cands := scan.FindCandidates(core, orc.Valid, align)
		if verbose {
			fmt.Fprintf(os.Stderr, "align=%d: %d candidate tables\n", align, len(cands))
		}
		res, err := orc.Evaluate(cands)
		if err == nil {
			return res, nil
		}
		if lastErr == nil {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("no validated collection table found — open the Collection tab and scroll through it, then retry\n%w", lastErr)
}
