package export

import (
	"os"
	"strings"
	"testing"

	"github.com/bynov/mtga-collection-export/internal/carddb"
)

func rows() []Row {
	return []Row{
		{Count: 4, Card: carddb.Card{GrpID: 7000, Name: "Heartfire Hero", Set: "BLB", CollectorNumber: "138", Rarity: "Mythic", Colors: "R", ManaCost: "{1}{R}"}},
		{Count: 1, Card: carddb.Card{GrpID: 90000, Name: "Heartfire Hero", Set: "MOM", CollectorNumber: "20", Rarity: "Mythic", Colors: "R", ManaCost: "{1}{R}"}},
		{Count: 5, Card: carddb.Card{GrpID: 7020, Name: "Forest", Set: "BLB", CollectorNumber: "280", Rarity: "Basic Land", Colors: "", IsBasicLand: true}},
	}
}

func TestReportCountsPrintingsNamesCopies(t *testing.T) {
	path := t.TempDir() + "/out.csv"
	rep, err := Write(path, "raw", rows())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Printings != 3 {
		t.Fatalf("printings = %d, want 3", rep.Printings)
	}
	if rep.DistinctNames != 2 { // two Heartfire printings share a name
		t.Fatalf("distinct names = %d, want 2", rep.DistinctNames)
	}
	if rep.Copies != 10 {
		t.Fatalf("copies = %d, want 10", rep.Copies)
	}
}

func TestRawFormatHeaderAndSorting(t *testing.T) {
	path := t.TempDir() + "/out.csv"
	if _, err := Write(path, "raw", rows()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if !strings.HasPrefix(lines[0], "count,name,set") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "Forest") { // Forest sorts before Heartfire
		t.Fatalf("first data row = %q", lines[1])
	}
}

func TestUnknownFormatErrors(t *testing.T) {
	if _, err := Write(t.TempDir()+"/x.csv", "bogus", rows()); err == nil {
		t.Fatal("expected error for unknown format")
	}
}
