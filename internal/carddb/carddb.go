// Package carddb resolves Arena grpIds to card details from the client's local
// SQLite card database, opened read-only on a copy.
package carddb

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

var rarityName = map[int]string{0: "", 1: "Basic Land", 2: "Common", 3: "Uncommon", 4: "Rare", 5: "Mythic"}

// colorLetter maps the fixed CardColor enum ints (handoff §6:
// 0=Colorless,1=White,2=Blue,3=Black,4=Red,5=Green,6=Land,7=Artifact) to WUBRG.
var colorLetter = map[int]string{1: "W", 2: "U", 3: "B", 4: "R", 5: "G"}

var tagRE = regexp.MustCompile(`<[^>]+>`)
var intRE = regexp.MustCompile(`\d+`)

// Card is a resolved printing.
type Card struct {
	GrpID           uint32
	Name, Set       string
	CollectorNumber string
	Rarity          string
	Colors          string
	TypeLine        string
	ManaCost        string
	IsBasicLand     bool
}

// DB wraps the read-only card database and its enum maps.
type DB struct {
	db                *sql.DB
	cardType, subType map[int]string
	superType         map[int]string
}

// Open opens the copied card DB read-only and loads enum maps.
func Open(path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	d := &DB{db: sqldb}
	for _, m := range []struct {
		kind string
		dst  *map[int]string
	}{
		{"CardType", &d.cardType}, {"SubType", &d.subType},
		{"SuperType", &d.superType},
	} {
		em, err := d.enumMap(m.kind)
		if err != nil {
			sqldb.Close()
			return nil, err
		}
		*m.dst = em
	}
	return d, nil
}

func (d *DB) enumMap(kind string) (map[int]string, error) {
	rows, err := d.db.Query(
		`SELECT e.Value, l.Loc FROM Enums e
		 JOIN Localizations_enUS l ON l.LocId = e.LocId
		 WHERE e.Type = ? GROUP BY e.Value`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var loc string
		if err := rows.Scan(&v, &loc); err != nil {
			return nil, err
		}
		out[v] = loc
	}
	return out, rows.Err()
}

func clean(s string) string { return strings.TrimSpace(tagRE.ReplaceAllString(s, "")) }

func splitIDs(raw string) []int {
	var out []int
	for _, m := range intRE.FindAllString(raw, -1) {
		if n, err := strconv.Atoi(m); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func manaCost(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, tok := range strings.Split(raw, "o") {
		if tok != "" {
			b.WriteString("{" + tok + "}")
		}
	}
	return b.String()
}

// Resolve returns the card for a grpId.
func (d *DB) Resolve(grpID uint32) (Card, bool) {
	var (
		name, expc, cnum, colors, types, subs, supers, mana sql.NullString
		rarity                                              sql.NullInt64
	)
	err := d.db.QueryRow(
		`SELECT l.Loc, c.ExpansionCode, c.CollectorNumber, c.Rarity,
		        c.Colors, c.Types, c.Subtypes, c.Supertypes, c.OldSchoolManaText
		 FROM Cards c LEFT JOIN Localizations_enUS l
		   ON l.LocId = c.TitleId AND l.Formatted = 1
		 WHERE c.GrpId = ?`, grpID).
		Scan(&name, &expc, &cnum, &rarity, &colors, &types, &subs, &supers, &mana)
	if err != nil {
		return Card{GrpID: grpID}, false
	}
	typeLine := strings.TrimSpace(strings.Join(append(
		names(d.superType, supers.String), names(d.cardType, types.String)...), " "))
	if st := names(d.subType, subs.String); len(st) > 0 {
		typeLine += " — " + strings.Join(st, " ")
	}
	var col strings.Builder
	for _, id := range splitIDs(colors.String) {
		col.WriteString(colorLetter[id])
	}
	return Card{
		GrpID:           grpID,
		Name:            clean(name.String),
		Set:             expc.String,
		CollectorNumber: cnum.String,
		Rarity:          rarityName[int(rarity.Int64)],
		Colors:          col.String(),
		TypeLine:        typeLine,
		ManaCost:        manaCost(mana.String),
		IsBasicLand:     rarity.Int64 == 1,
	}, true
}

func names(m map[int]string, raw string) []string {
	var out []string
	for _, id := range splitIDs(raw) {
		if n := m[id]; n != "" {
			out = append(out, n)
		}
	}
	return out
}

func (d *DB) idSet(query string) map[uint32]bool {
	out := map[uint32]bool{}
	rows, err := d.db.Query(query)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id uint32
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// Rebalanced returns grpIds with IsRebalanced=1 (Alchemy twins to drop).
func (d *DB) Rebalanced() map[uint32]bool {
	return d.idSet(`SELECT GrpId FROM Cards WHERE IsRebalanced = 1`)
}

// Basics returns rarity-1 basic-land grpIds.
func (d *DB) Basics() map[uint32]bool {
	return d.idSet(`SELECT GrpId FROM Cards WHERE Rarity = 1`)
}

// Collectible returns primary, non-token grpIds.
func (d *DB) Collectible() map[uint32]bool {
	return d.idSet(`SELECT GrpId FROM Cards WHERE IsPrimaryCard = 1 AND IsToken = 0`)
}

// Close releases the database.
func (d *DB) Close() error { return d.db.Close() }
