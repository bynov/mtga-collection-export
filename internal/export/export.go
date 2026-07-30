// Package export writes a validated collection to CSV and reports totals.
package export

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/bynov/mtga-collection-export/internal/carddb"
)

// Row is one printing with its owned count.
type Row struct {
	Count uint32
	Card  carddb.Card
}

// Report is the honest three-figure summary (handoff §6).
type Report struct {
	Printings     int
	DistinctNames int
	Copies        uint32
}

// Write emits rows in the given format and returns the totals.
func Write(path, format string, rows []Row) (Report, error) {
	header, project, err := formatter(format)
	if err != nil {
		return Report{}, err
	}
	sorted := append([]Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Card.Name != sorted[j].Card.Name {
			return sorted[i].Card.Name < sorted[j].Card.Name
		}
		return sorted[i].Card.Set < sorted[j].Card.Set
	})

	f, err := os.Create(path)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return Report{}, err
	}
	names := map[string]bool{}
	var rep Report
	for _, r := range sorted {
		if err := w.Write(project(r)); err != nil {
			return Report{}, err
		}
		rep.Printings++
		rep.Copies += r.Count
		names[r.Card.Name] = true
	}
	rep.DistinctNames = len(names)
	w.Flush()
	return rep, w.Error()
}

func formatter(format string) ([]string, func(Row) []string, error) {
	u := func(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
	switch format {
	case "raw":
		return []string{"count", "name", "set", "collector_number", "rarity", "colors", "type", "mana_cost", "arena_id"},
			func(r Row) []string {
				return []string{
					u(r.Count), r.Card.Name, r.Card.Set, r.Card.CollectorNumber,
					r.Card.Rarity, r.Card.Colors, r.Card.TypeLine, r.Card.ManaCost, u(r.Card.GrpID),
				}
			}, nil
	case "moxfield":
		return []string{"Count", "Name", "Edition", "Collector Number"},
			func(r Row) []string {
				return []string{u(r.Count), r.Card.Name, r.Card.Set, r.Card.CollectorNumber}
			}, nil
	case "manabox":
		return []string{"Name", "Set code", "Collector number", "Quantity"},
			func(r Row) []string {
				return []string{r.Card.Name, r.Card.Set, r.Card.CollectorNumber, u(r.Count)}
			}, nil
	default:
		return nil, nil, fmt.Errorf("unknown format %q (want raw, moxfield, or manabox)", format)
	}
}
