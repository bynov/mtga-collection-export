package starthook

import "testing"

// Fixture mirrors the real StartHook schema: Decks is a GUID-keyed map, and
// classification data lives in DeckSummaries with lowercase attribute keys and
// a quote-wrapped LastPlayed value.
const fixture = `{
  "CardMetadataInfo": {"NonCollectibleCardList": [100, 200]},
  "Decks": {
    "guid-played":   {"MainDeck": [{"cardId": 7000, "quantity": 4}, {"cardId": 7010, "quantity": 2}]},
    "guid-precon":   {"MainDeck": [{"cardId": 500, "quantity": 4}]},
    "guid-imported": {"MainDeck": [{"cardId": 87145, "quantity": 4}]}
  },
  "DeckSummaries": [
    {"DeckId": "guid-played", "Name": "My Mono Red",
     "Attributes": [{"name": "LastPlayed", "value": "\"2026-07-20T10:00:00+02:00\""}]},
    {"DeckId": "guid-precon", "Name": "?=?Loc/Decks/Precon/RNA_Azorius",
     "Attributes": [{"name": "Format", "value": "Standard"}]},
    {"DeckId": "guid-imported", "Name": "Imported Deck",
     "Attributes": [{"name": "LastPlayed", "value": "\"0001-01-01T00:00:00\""}]}
  ]
}`

func TestNonCollectible(t *testing.T) {
	s, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	nc := s.NonCollectible()
	if !nc[100] || !nc[200] || len(nc) != 2 {
		t.Fatalf("NonCollectible = %v", nc)
	}
}

func TestPlayedDeckCardsExcludesPreconAndImported(t *testing.T) {
	s, _ := Parse([]byte(fixture))
	owned := s.PlayedDeckCards()
	if owned[7000] != 4 || owned[7010] != 2 {
		t.Fatalf("played deck cards not derived: %v", owned)
	}
	if _, ok := owned[500]; ok {
		t.Fatal("precon card leaked into owned")
	}
	if _, ok := owned[87145]; ok {
		t.Fatal("imported-deck (0001-01-01 LastPlayed) card leaked into owned")
	}
}
