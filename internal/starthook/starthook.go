// Package starthook parses Arena's StartHook JSON to bootstrap the oracle:
// proven-owned cards from played decks and the non-collectible filter list.
//
// Schema (verified against a real Player.log, 2026-07):
//   - Decks: map keyed by deck GUID -> {MainDeck: [{cardId, quantity}], ...}
//   - DeckSummaries: list of {DeckId, Name, Attributes: [{name, value}]}.
//     A deck is "played" (proves ownership) when its Name is not a precon
//     (`?=?Loc/Decks/Precon/` prefix) and its LastPlayed attribute is a real
//     date. Attribute keys are lowercase; the LastPlayed value is itself a
//     quoted JSON string, e.g. `"2026-07-29T18:30:50+02:00"`.
//   - CardMetadataInfo.NonCollectibleCardList: grpIds to filter out.
package starthook

import (
	"encoding/json"
	"strings"
)

type rawHook struct {
	CardMetadataInfo struct {
		NonCollectibleCardList []uint32 `json:"NonCollectibleCardList"`
	} `json:"CardMetadataInfo"`
	Decks         map[string]rawDeck `json:"Decks"`
	DeckSummaries []rawSummary       `json:"DeckSummaries"`
}

type rawDeck struct {
	MainDeck []cardEntry `json:"MainDeck"`
}

type cardEntry struct {
	CardID   uint32 `json:"cardId"`
	Quantity uint32 `json:"quantity"`
}

type rawSummary struct {
	DeckId     string `json:"DeckId"`
	Name       string `json:"Name"`
	Attributes []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"Attributes"`
}

// StartHook is the parsed payload.
type StartHook struct{ raw rawHook }

// Parse decodes a StartHook JSON object.
func Parse(raw []byte) (*StartHook, error) {
	var h rawHook
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &StartHook{raw: h}, nil
}

// NonCollectible returns the non-collectible grpId set.
func (s *StartHook) NonCollectible() map[uint32]bool {
	out := map[uint32]bool{}
	for _, id := range s.raw.CardMetadataInfo.NonCollectibleCardList {
		out[id] = true
	}
	return out
}

// lastPlayed returns the trimmed LastPlayed value, or "" if absent.
func (sm rawSummary) lastPlayed() string {
	for _, a := range sm.Attributes {
		if a.Name == "LastPlayed" {
			return strings.Trim(a.Value, `"`)
		}
	}
	return ""
}

// isPlayed reports whether a summary is a non-precon deck with a real LastPlayed.
func (sm rawSummary) isPlayed() bool {
	if strings.HasPrefix(sm.Name, "?=?Loc/Decks/Precon/") {
		return false
	}
	lp := sm.lastPlayed()
	return lp != "" && !strings.HasPrefix(lp, "0001-01-01")
}

// PlayedDeckCards returns grpId -> max copies across played decks. A deck
// running 4x a card proves ownership of at least 4.
func (s *StartHook) PlayedDeckCards() map[uint32]uint32 {
	out := map[uint32]uint32{}
	for _, sm := range s.raw.DeckSummaries {
		if !sm.isPlayed() {
			continue
		}
		deck, ok := s.raw.Decks[sm.DeckId]
		if !ok {
			continue
		}
		for _, card := range deck.MainDeck {
			if card.CardID == 0 {
				continue
			}
			if card.Quantity > out[card.CardID] {
				out[card.CardID] = card.Quantity
			}
		}
	}
	return out
}
