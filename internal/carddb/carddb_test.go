package carddb

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func fixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "card.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE Cards (GrpId INTEGER, TitleId INTEGER, ExpansionCode TEXT,
			CollectorNumber TEXT, Rarity INTEGER, Colors TEXT, Types TEXT,
			Subtypes TEXT, Supertypes TEXT, OldSchoolManaText TEXT,
			IsPrimaryCard INTEGER, IsToken INTEGER, IsRebalanced INTEGER)`,
		`CREATE TABLE Localizations_enUS (LocId INTEGER, Loc TEXT, Formatted INTEGER)`,
		`CREATE TABLE Enums (Type TEXT, Value INTEGER, LocId INTEGER)`,
		// grpId 7000: Heartfire Hero, red, Creature, {1}{R}, rarity Mythic
		`INSERT INTO Cards VALUES (7000, 1, 'BLB', '138', 5, '4', '1', '', '', 'o1oR', 1, 0, 0)`,
		`INSERT INTO Localizations_enUS VALUES (1, '<nobr>Heartfire</nobr> Hero', 1)`,
		// grpId 7001: rebalanced twin of 7000 — must be filtered
		`INSERT INTO Cards VALUES (7001, 1, 'BLB', '138', 5, '4', '1', '', '', 'o1oR', 1, 0, 1)`,
		// grpId 7020: Forest, basic land
		`INSERT INTO Cards VALUES (7020, 2, 'BLB', '280', 1, '6', '1', '3', '', '', 1, 0, 0)`,
		`INSERT INTO Localizations_enUS VALUES (2, 'Forest', 1)`,
		// grpId 999: a token — not collectible
		`INSERT INTO Cards VALUES (999, 3, 'BLB', 'T1', 2, '', '1', '', '', '', 1, 1, 0)`,
		`INSERT INTO Localizations_enUS VALUES (3, 'Beast Token', 1)`,
		// enums
		`INSERT INTO Enums VALUES ('CardType', 1, 10)`,
		`INSERT INTO Localizations_enUS VALUES (10, 'Creature', 1)`,
		`INSERT INTO Enums VALUES ('SubType', 3, 11)`,
		`INSERT INTO Localizations_enUS VALUES (11, 'Forest', 1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	return path
}

func TestResolveBuildsCardFields(t *testing.T) {
	db, err := Open(fixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c, ok := db.Resolve(7000)
	if !ok {
		t.Fatal("7000 not resolved")
	}
	if c.Name != "Heartfire Hero" {
		t.Fatalf("markup not stripped: %q", c.Name)
	}
	if c.Rarity != "Mythic" {
		t.Fatalf("rarity = %q", c.Rarity)
	}
	if c.ManaCost != "{1}{R}" {
		t.Fatalf("mana = %q", c.ManaCost)
	}
	if c.Colors != "R" {
		t.Fatalf("colors = %q", c.Colors)
	}
}

func TestRebalancedAndBasicsAndCollectible(t *testing.T) {
	db, _ := Open(fixtureDB(t))
	defer db.Close()
	if !db.Rebalanced()[7001] {
		t.Fatal("7001 should be flagged rebalanced")
	}
	if !db.Basics()[7020] {
		t.Fatal("7020 (Forest) should be a basic")
	}
	col := db.Collectible()
	if !col[7000] || col[999] {
		t.Fatalf("collectible filter wrong: 7000=%v 999(token)=%v", col[7000], col[999])
	}
}
