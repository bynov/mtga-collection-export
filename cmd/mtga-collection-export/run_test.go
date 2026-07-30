package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bynov/mtga-collection-export/internal/testcore"
	_ "modernc.org/sqlite"
)

// buildCardDB writes a card DB covering the planted valid ids. The `basics` set
// is marked rarity 1 so the oracle's >4-copy cap does not reject them (the
// planted collection stores basics at 5-40 copies).
func buildCardDB(t *testing.T, valid []uint32, basics map[uint32]bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "carddb.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%v: %s", err, q)
		}
	}
	must(`CREATE TABLE Cards (GrpId INTEGER, TitleId INTEGER, ExpansionCode TEXT,
		CollectorNumber TEXT, Rarity INTEGER, Colors TEXT, Types TEXT, Subtypes TEXT,
		Supertypes TEXT, OldSchoolManaText TEXT, IsPrimaryCard INTEGER, IsToken INTEGER, IsRebalanced INTEGER)`)
	must(`CREATE TABLE Localizations_enUS (LocId INTEGER, Loc TEXT, Formatted INTEGER)`)
	must(`CREATE TABLE Enums (Type TEXT, Value INTEGER, LocId INTEGER)`)
	tx, _ := db.Begin()
	st, _ := tx.Prepare(`INSERT INTO Cards VALUES (?,?,?,?,?,?,?,?,?,?,1,0,0)`)
	ln, _ := tx.Prepare(`INSERT INTO Localizations_enUS VALUES (?,?,1)`)
	for i, g := range valid {
		rarity := 2 // common
		if basics[g] {
			rarity = 1 // basic land
		}
		st.Exec(g, i+1, "TST", fmt.Sprintf("%d", i+1), rarity, "1", "1", "", "", "o1")
		ln.Exec(i+1, fmt.Sprintf("Card %d", g))
	}
	tx.Commit()
	return path
}

// writeStartHook writes a minimal real-schema StartHook whose one played deck
// proves ownership of the given cards.
func writeStartHook(t *testing.T, owned map[uint32]uint32) string {
	t.Helper()
	var md []string
	for g, n := range owned {
		md = append(md, fmt.Sprintf(`{"cardId":%d,"quantity":%d}`, g, n))
	}
	js := fmt.Sprintf(`{
	  "CardMetadataInfo": {"NonCollectibleCardList": []},
	  "Decks": {"g1": {"MainDeck": [%s]}},
	  "DeckSummaries": [
	    {"DeckId":"g1","Name":"Test Deck",
	     "Attributes":[{"name":"LastPlayed","value":"\"2026-07-20T10:00:00+02:00\""}]}
	  ]
	}`, strings.Join(md, ","))
	path := filepath.Join(t.TempDir(), "starthook.json")
	if err := os.WriteFile(path, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeValid() []uint32 {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	return valid
}

func TestRunEndToEndOnSyntheticCore(t *testing.T) {
	valid := makeValid()
	owned := map[uint32]uint32{7000: 4, 7010: 2, 7020: 1}
	b, err := testcore.Build(t.TempDir(), testcore.Spec{
		Valid: valid, Owned: owned, Absent: []uint32{9999999}, Seed: 1337,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "collection.csv")
	err = run(Options{
		Core:          b.CorePath,
		Out:           out,
		Format:        "raw",
		CardDB:        buildCardDB(t, valid, b.Basics),
		StartHookPath: writeStartHook(t, owned),
		Absent:        []uint32{9999999},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// The CSV must contain every planted card with its exact count, and never
	// the known-absent id.
	got := readCSV(t, out)
	for g, n := range b.Truth {
		if got[g] != n {
			t.Fatalf("card %d: got count %d, planted %d", g, got[g], n)
		}
	}
	if _, ok := got[9999999]; ok {
		t.Fatal("known-absent card leaked into output")
	}
}

func TestRunRefusesWhenNothingValidates(t *testing.T) {
	valid := []uint32{6873}
	b, _ := testcore.Build(t.TempDir(), testcore.Spec{Valid: valid, Owned: map[uint32]uint32{}, Seed: 1})
	err := run(Options{
		Core:   b.CorePath,
		Out:    filepath.Join(t.TempDir(), "x.csv"),
		Format: "raw",
		CardDB: buildCardDB(t, valid, nil),
	})
	if err == nil {
		t.Fatal("run must refuse to emit when nothing validates")
	}
}

// readCSV maps arena_id -> count from a raw-format export.
func readCSV(t *testing.T, path string) map[uint32]uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]uint32{}
	for _, r := range recs[1:] { // skip header
		var id, cnt uint32
		fmt.Sscan(r[0], &cnt)
		fmt.Sscan(r[8], &id)
		out[id] = cnt
	}
	return out
}
