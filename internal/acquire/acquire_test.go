package acquire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLldbArgsUsesFullStyleAndDetaches(t *testing.T) {
	args := lldbArgs(4242, "/tmp/out.core")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-p 4242") {
		t.Fatalf("pid missing: %v", args)
	}
	if !strings.Contains(joined, "process save-core --style full") {
		t.Fatalf("must use --style full: %v", args)
	}
	if !strings.Contains(joined, "/tmp/out.core") || !strings.Contains(joined, "detach") {
		t.Fatalf("out path or detach missing: %v", args)
	}
}

func TestExtractStartHookReturnsLastObject(t *testing.T) {
	log := filepath.Join(t.TempDir(), "Player.log")
	// Real format: `<== StartHook(<uuid>)` then JSON on the NEXT line.
	content := strings.Join([]string{
		`[UnityCrossThreadLogger]==> StartHook(a) {"junk":1}`,
		`[UnityCrossThreadLogger]<== StartHook(b)`,
		`{"InventoryInfo":{"a":1},"Decks":{}}`,
		`some other line`,
		`[UnityCrossThreadLogger]<== StartHook(c)`,
		`{"InventoryInfo":{"a":2},"Decks":{"g":{"MainDeck":[]}}}`,
	}, "\n")
	os.WriteFile(log, []byte(content), 0o644)

	got, err := ExtractStartHook(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"a":2`) {
		t.Fatalf("should return the LAST StartHook: %s", got)
	}
	if strings.Contains(string(got), "StartHook") {
		t.Fatalf("marker not stripped: %s", got)
	}
	if got[0] != '{' || got[len(got)-1] != '}' {
		t.Fatalf("not a balanced object: %s", got)
	}
}

func TestExtractStartHookHandlesEmbeddedBraces(t *testing.T) {
	log := filepath.Join(t.TempDir(), "Player.log")
	// A quote-wrapped value containing braces must not fool the balancer.
	content := "<== StartHook(x)\n" + `{"Name":"deck {with braces}","Decks":{"g":{}}}`
	os.WriteFile(log, []byte(content), 0o644)
	got, err := ExtractStartHook(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), `"Decks":{"g":{}}}`) {
		t.Fatalf("balancer stopped early: %s", got)
	}
}

func TestCopyCardDB(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Raw_CardDatabase_abc.mtga")
	os.WriteFile(src, []byte("SQLite format 3\x00rest"), 0o644)
	dstDir := t.TempDir()
	dst, err := CopyCardDB(src, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if !strings.HasPrefix(string(b), "SQLite format 3") {
		t.Fatalf("copy corrupt: %q", b[:16])
	}
}
