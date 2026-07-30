package starthook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRealPlayerLog validates parsing against the actual Player.log when present.
// Skipped in CI / on machines without Arena.
func TestRealPlayerLog(t *testing.T) {
	home, _ := os.UserHomeDir()
	log := filepath.Join(home, "Library", "Logs", "Wizards Of The Coast", "MTGA", "Player.log")
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Skip("no real Player.log")
	}
	// crude extraction mirroring acquire.ExtractStartHook to avoid an import cycle
	s := string(raw)
	m := lastIndex(s, "<== StartHook")
	if m < 0 {
		t.Skip("no StartHook in real log")
	}
	start := indexByte(s[m:], '{') + m
	obj := balanced(s[start:])
	sh, err := Parse([]byte(obj))
	if err != nil {
		t.Fatalf("real StartHook did not parse: %v", err)
	}
	owned := sh.PlayedDeckCards()
	nc := sh.NonCollectible()
	t.Logf("real log: %d proven-owned cards, %d non-collectible ids", len(owned), len(nc))
	if len(nc) == 0 {
		t.Errorf("expected a non-collectible list in the real StartHook")
	}
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
func balanced(s string) string {
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}
