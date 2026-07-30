# mtga-collection-export Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A read-only, offline Go CLI that exports a macOS MTG Arena player's collection (card identity + owned count) to CSV, refusing to emit anything that fails validation.

**Architecture:** Three sharply separated layers ported from the handoff prototype: L1 `acquire` (macOS-only lldb core dump + file location), L2 `machocore`/`scan`/`oracle` (portable, assumption-free discovery + validation), L3 `carddb`/`export` (portable SQLite resolution + CSV). The scan re-derives the collection table's layout every run and the oracle gates all output — nothing is hardcoded.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure-Go, cgo-free), Apple `lldb` (invoked as a subprocess), standard library only otherwise.

## Global Constraints

- **Module path:** `github.com/bynov/mtga-collection-export`.
- **Go version floor:** `go 1.26` in `go.mod`.
- **cgo-free:** never import a cgo package; SQLite is `modernc.org/sqlite` only. `CGO_ENABLED=0` must build the binary.
- **Read-only / offline:** no network calls, no writes to any game file, no process injection. The only external process invoked is `/usr/bin/lldb`, and only for `process save-core` + `detach`.
- **Never hardcode addresses, offsets, or strides** — re-derive every run (handoff §9).
- **Oracle-gated output:** never write a CSV that failed validation; on failure print the verdict + evidence and exit non-zero (handoff §9).
- **Report three figures** on success: printings/rows, distinct card names, total copies (handoff §6). Log every filter/cap applied.
- **License:** MIT. README must carry the WotC non-affiliation disclaimer (handoff §11).
- **Commits:** conventional-commit style, one per task step group. Use `git commit --no-gpg-sign` (interactive GPG pinentry is unavailable in this environment).
- **Layout constants** (handoff §5, verbatim): `STRIDES = {8,12,16,20,24,28,32,40,48,64}`, `COUNT_OFFSETS = {-16,-12,-8,-4,4,8,12,16}`, `MAX_PLAUSIBLE_COUNT = 4` for non-basics, cluster `maxGap = 512` bytes, `minHits = 60`, `LC_SEGMENT_64 = 0x19`, `MH_MAGIC_64 = 0xFEEDFACF`, writable = `initprot & 0x2`.

---

## File Structure

```
go.mod
cmd/mtga-collection-export/main.go              CLI: flags, orchestration, verdict printing
internal/
  machocore/
    core.go        Open/Close, LC_SEGMENT_64 parse, writable-segment index, ReadU32
    core_test.go
  testcore/
    build.go       synthetic Mach-O core writer (planted collection + decoys) for tests
    build_test.go
  scan/
    scan.go        LUT + sighting scan + cluster + residue-run + candidate assembly
    scan_test.go
  starthook/
    starthook.go   parse StartHook JSON: played-deck cards, NonCollectibleCardList
    starthook_test.go
  oracle/
    oracle.go      structural + oracle scoring, winner selection, element-wise-max reconcile
    oracle_test.go
  carddb/
    carddb.go      open copied SQLite read-only, resolve grpId->Card, rebalanced/basics/collectible sets
    carddb_test.go
  export/
    export.go      CSV writers (raw/moxfield/manabox), printings/names/copies report
    export_test.go
  acquire/
    acquire.go     locate pid + files, lldb DumpCore, ExtractStartHook, copy card DB
    acquire_test.go
docs/superpowers/specs/2026-07-30-mtga-collection-export-design.md   (exists)
README.md
LICENSE
```

Each `internal/*` package has one responsibility and is unit-tested against synthetic fixtures — no running game required except the optional manual end-to-end path in Task 11.

---

## Task 1: Project scaffold

**Files:**
- Create: `go.mod`
- Create: `internal/version/version_test.go`, `internal/version/version.go`

**Interfaces:**
- Produces: module `github.com/bynov/mtga-collection-export`; `version.String` constant.

- [ ] **Step 1: Init module**

```bash
cd /Users/bynov/go/src/github.com/bynov/mtga-collection-export
go mod init github.com/bynov/mtga-collection-export
go mod edit -go=1.26
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Write a trivial failing test to confirm the toolchain**

`internal/version/version_test.go`:

```go
package version

import "testing"

func TestStringIsSet(t *testing.T) {
	if String == "" {
		t.Fatal("version.String must not be empty")
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/version/`
Expected: FAIL — `undefined: String`.

- [ ] **Step 4: Implement**

`internal/version/version.go`:

```go
// Package version holds the build version string.
package version

// String is the tool version, overridable at build time with -ldflags.
var String = "0.1.0-dev"
```

- [ ] **Step 5: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./internal/version/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/version
git commit --no-gpg-sign -m "chore: scaffold go module and version package"
```

---

## Task 2: `machocore` — parse a Mach-O core and read words

**Files:**
- Create: `internal/machocore/core.go`
- Test: `internal/machocore/core_test.go`

**Interfaces:**
- Produces:
  - `type Segment struct { VMAddr, FileOff, Size uint64; InitProt uint32 }`
  - `func Open(path string) (*Core, error)`
  - `func (c *Core) Segments() []Segment` — writable segments only, sorted by VMAddr
  - `func (c *Core) ReadU32(addr uint64) (uint32, bool)` — LE word, `ok=false` if unmapped
  - `func (c *Core) Close() error`

Handoff pitfalls pre-solved: after the 8-byte `cmd`/`cmdsize`, the segment body is 64 bytes; the fields are `body[16:56]` unpacked `<QQQQii` (vmaddr, vmsize, fileoff, filesize, maxprot, initprot). Slicing `[16:64]` is a struct-size error (§7.1). Segment lookup is binary search over VMAddr-sorted segments, never linear (§7.2).

- [ ] **Step 1: Write the failing test**

This test hand-builds a 1-segment core (2 words) in memory, writes it to a temp file, and asserts parsing + reads. Put the tiny core builder inline; the full decoy builder is Task 3.

`internal/machocore/core_test.go`:

```go
package machocore

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalCore writes a Mach-O core with one rw- segment containing `data`
// mapped at vmBase. Header layout mirrors the real 64-bit core.
func minimalCore(t *testing.T, vmBase uint64, data []byte) string {
	t.Helper()
	const ncmds = 1
	hdr := make([]byte, 32)
	binary.LittleEndian.PutUint32(hdr[0:], 0xFEEDFACF) // magic
	binary.LittleEndian.PutUint32(hdr[4:], 0x0100000C) // cputype arm64
	binary.LittleEndian.PutUint32(hdr[8:], 0)          // cpusubtype
	binary.LittleEndian.PutUint32(hdr[12:], 4)         // MH_CORE
	binary.LittleEndian.PutUint32(hdr[16:], ncmds)
	binary.LittleEndian.PutUint32(hdr[20:], 72*ncmds)
	fileoff := uint64(32 + 72*ncmds)

	seg := make([]byte, 72)
	binary.LittleEndian.PutUint32(seg[0:], 0x19) // LC_SEGMENT_64
	binary.LittleEndian.PutUint32(seg[4:], 72)   // cmdsize
	// seg[8:24] segname = zeros
	binary.LittleEndian.PutUint64(seg[24:], vmBase)          // vmaddr
	binary.LittleEndian.PutUint64(seg[32:], uint64(len(data))) // vmsize
	binary.LittleEndian.PutUint64(seg[40:], fileoff)          // fileoff
	binary.LittleEndian.PutUint64(seg[48:], uint64(len(data))) // filesize
	binary.LittleEndian.PutUint32(seg[56:], 0x7)             // maxprot rwx
	binary.LittleEndian.PutUint32(seg[60:], 0x3)             // initprot rw-

	path := filepath.Join(t.TempDir(), "min.core")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(hdr)
	f.Write(seg)
	f.Write(data)
	return path
}

func TestOpenReadsWritableSegmentWords(t *testing.T) {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[0:], 0xAABBCCDD)
	binary.LittleEndian.PutUint32(data[4:], 42)
	base := uint64(0x200000000)

	c, err := Open(minimalCore(t, base, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if got := c.Segments(); len(got) != 1 || got[0].VMAddr != base {
		t.Fatalf("segments = %+v", got)
	}
	if v, ok := c.ReadU32(base); !ok || v != 0xAABBCCDD {
		t.Fatalf("ReadU32(base) = %#x, %v", v, ok)
	}
	if v, ok := c.ReadU32(base + 4); !ok || v != 42 {
		t.Fatalf("ReadU32(base+4) = %d, %v", v, ok)
	}
	if _, ok := c.ReadU32(base + 8); ok {
		t.Fatal("read past segment end should fail")
	}
	if _, ok := c.ReadU32(base - 4); ok {
		t.Fatal("read before segment should fail")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/machocore/`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Implement**

`internal/machocore/core.go`:

```go
// Package machocore parses a Mach-O core dump and exposes read-only access to
// its writable segments. It assumes nothing about content — the scan layer does.
package machocore

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

const (
	lcSegment64 = 0x19
	mhMagic64   = 0xFEEDFACF
	vmProtWrite = 0x2
)

// Segment is one writable LC_SEGMENT_64 region.
type Segment struct {
	VMAddr, FileOff, Size uint64
	InitProt              uint32
}

// Core is a memory-mapped core dump. Not safe for concurrent use.
type Core struct {
	f     *os.File
	data  []byte // full file, mmap-like via os.ReadFile is acceptable; see note
	segs  []Segment
	start []uint64 // segs' VMAddr, for binary search
}

// Open parses the Mach-O header and indexes writable segments.
func Open(path string) (*Core, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 32)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		f.Close()
		return nil, err
	}
	if binary.LittleEndian.Uint32(hdr[0:]) != mhMagic64 {
		f.Close()
		return nil, fmt.Errorf("not a 64-bit Mach-O core: magic=%#x", binary.LittleEndian.Uint32(hdr[0:]))
	}
	ncmds := binary.LittleEndian.Uint32(hdr[16:])

	var segs []Segment
	off := int64(32)
	head := make([]byte, 8)
	for i := uint32(0); i < ncmds; i++ {
		if _, err := f.ReadAt(head, off); err != nil {
			break
		}
		cmd := binary.LittleEndian.Uint32(head[0:])
		cmdsize := binary.LittleEndian.Uint32(head[4:])
		if cmdsize < 8 {
			f.Close()
			return nil, fmt.Errorf("bad cmdsize %d at cmd %d", cmdsize, i)
		}
		if cmd == lcSegment64 {
			body := make([]byte, cmdsize-8)
			if _, err := f.ReadAt(body, off+8); err != nil {
				break
			}
			// body[16:56] = vmaddr, vmsize, fileoff, filesize (u64) + maxprot, initprot (i32)
			vmaddr := binary.LittleEndian.Uint64(body[16:])
			vmsize := binary.LittleEndian.Uint64(body[24:])
			fileoff := binary.LittleEndian.Uint64(body[32:])
			filesize := binary.LittleEndian.Uint64(body[40:])
			initprot := binary.LittleEndian.Uint32(body[52:])
			if filesize > 0 && initprot&vmProtWrite != 0 {
				size := vmsize
				if filesize < size {
					size = filesize
				}
				segs = append(segs, Segment{VMAddr: vmaddr, FileOff: fileoff, Size: size, InitProt: initprot})
			}
		}
		off += int64(cmdsize)
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].VMAddr < segs[j].VMAddr })
	starts := make([]uint64, len(segs))
	for i, s := range segs {
		starts[i] = s.VMAddr
	}
	return &Core{f: f, segs: segs, start: starts}, nil
}

// Segments returns writable segments sorted by VMAddr.
func (c *Core) Segments() []Segment { return c.segs }

// SegmentBytes reads a segment's mapped bytes.
func (c *Core) SegmentBytes(s Segment) ([]byte, error) {
	buf := make([]byte, s.Size)
	_, err := c.f.ReadAt(buf, int64(s.FileOff))
	return buf, err
}

// ReadU32 reads a little-endian 32-bit word at a virtual address.
func (c *Core) ReadU32(addr uint64) (uint32, bool) {
	i := sort.Search(len(c.start), func(k int) bool { return c.start[k] > addr })
	i--
	if i < 0 {
		return 0, false
	}
	s := c.segs[i]
	rel := addr - s.VMAddr
	if rel+4 > s.Size {
		return 0, false
	}
	var w [4]byte
	if _, err := c.f.ReadAt(w[:], int64(s.FileOff+rel)); err != nil {
		return 0, false
	}
	return binary.LittleEndian.Uint32(w[:]), true
}

// Close releases the file handle.
func (c *Core) Close() error { return c.f.Close() }
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/machocore/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/machocore
git commit --no-gpg-sign -m "feat(machocore): parse core segments and read words"
```

---

## Task 3: `testcore` — synthetic core with planted collection + decoys

Port of `make_fake_core.py`. This is the primary correctness gate (handoff §7.9, §9.1): a core with a planted collection plus four decoys (full card DB, precon decklist, craftability int[], and a decoy containing known-absent cards). The scanner must recover the collection and reject every decoy.

**Files:**
- Create: `internal/testcore/build.go`
- Test: `internal/testcore/build_test.go`

**Interfaces:**
- Consumes: `machocore.Open`, `machocore.ReadU32` (Task 2).
- Produces:
  - `type Spec struct { Valid []uint32; Owned map[uint32]uint32; Absent []uint32; Seed int64 }`
  - `type Built struct { CorePath string; Truth map[uint32]uint32; Basics map[uint32]bool }`
  - `func Build(dir string, s Spec) (Built, error)` — writes `dir/fake.core`, returns planted ground truth. Deterministic given `Seed` (uses `math/rand.NewSource`, never the global RNG or wall clock).

Implementation notes carried from the prototype:
- Collection table = `dict_entries` with stride 16, key at +8, value at +12, 0.75 fill (empty slots scattered).
- Decoys: `card_db_rows` (stride 48, neighbouring word is a random non-count), `precon` (`dict_entries` stride 24, key@0 val@4), `craftable` (flat int[] stride 4), and `precon_with_getlost` (a `dict_entries` table that DOES contain every `Absent` id — this is what the oracle rejects on).
- Ground truth: all `Owned`, plus ~1800 extra valid ids (none absent), with 9 entries bumped to 5–40 to emulate basic lands exceeding 4. Return those 9 as `Basics`.
- Segments are 16-byte aligned with random padding; header exactly as in `make_fake_core.py` (magic `0xFEEDFACF`, cputype `0x0100000C`, `MH_CORE=4`, 72-byte `LC_SEGMENT_64` cmds, initprot `0x3`).

- [ ] **Step 1: Write the failing test**

`internal/testcore/build_test.go`:

```go
package testcore

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/machocore"
)

func smallSpec() Spec {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	return Spec{
		Valid:  valid,
		Owned:  map[uint32]uint32{7000: 4, 7010: 2, 7020: 1},
		Absent: []uint32{9999999, 9999998}, // not in Valid, planted only in the decoy
		Seed:   1337,
	}
}

func TestBuildIsParseableAndPlantsTruth(t *testing.T) {
	b, err := Build(t.TempDir(), smallSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Truth) == 0 {
		t.Fatal("no planted truth")
	}
	for _, g := range []uint32{7000, 7010, 7020} {
		if _, ok := b.Truth[g]; !ok {
			t.Fatalf("owned card %d missing from planted truth", g)
		}
	}
	c, err := machocore.Open(b.CorePath)
	if err != nil {
		t.Fatalf("built core not parseable: %v", err)
	}
	defer c.Close()
	if len(c.Segments()) < 5 {
		t.Fatalf("expected >=5 segments (collection + 4 decoys), got %d", len(c.Segments()))
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, _ := Build(t.TempDir(), smallSpec())
	b, _ := Build(t.TempDir(), smallSpec())
	if len(a.Truth) != len(b.Truth) {
		t.Fatalf("nondeterministic truth size: %d vs %d", len(a.Truth), len(b.Truth))
	}
	for k, v := range a.Truth {
		if b.Truth[k] != v {
			t.Fatalf("nondeterministic truth for %d: %d vs %d", k, v, b.Truth[k])
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/testcore/`
Expected: FAIL — `undefined: Build`.

- [ ] **Step 3: Implement**

`internal/testcore/build.go`:

```go
// Package testcore builds synthetic Mach-O core dumps for scanner tests: a
// planted collection table plus decoys mimicking Arena's heap. Deterministic
// given Spec.Seed — no wall clock, no global RNG.
package testcore

import (
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

const (
	lcSegment64 = 0x19
	segBase     = 0x200000000
	segStride   = 4 * 1024 * 1024
)

// Spec describes the collection and decoys to plant.
type Spec struct {
	Valid  []uint32
	Owned  map[uint32]uint32
	Absent []uint32
	Seed   int64
}

// Built is the result of Build: the core path and its ground truth.
type Built struct {
	CorePath string
	Truth    map[uint32]uint32
	Basics   map[uint32]bool
}

// dictEntries emulates Dictionary<int,int>: {hashCode, next, key, value} slots
// with scattered empties. stride/keyOff/valOff let us vary the shape per table.
func dictEntries(rng *rand.Rand, pairs map[uint32]uint32, stride, keyOff, valOff int) []byte {
	keys := make([]uint32, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	nslots := int(float64(len(keys)) / 0.75)
	if nslots < len(keys) {
		nslots = len(keys)
	}
	free := rng.Perm(nslots)
	slot := make([]int, nslots)
	for i := range slot {
		slot[i] = -1
	}
	for i, k := range keys {
		slot[free[i]] = int(k)
	}
	kv := make(map[int]uint32, len(pairs))
	for k, v := range pairs {
		kv[int(k)] = v
	}
	buf := make([]byte, 0, nslots*stride)
	for _, s := range slot {
		rec := make([]byte, stride)
		if s >= 0 {
			binary.LittleEndian.PutUint32(rec[0:], uint32(s)&0x7FFFFFFF) // hash
			binary.LittleEndian.PutUint32(rec[4:], 0xFFFFFFFF)           // next = -1
			binary.LittleEndian.PutUint32(rec[keyOff:], uint32(s))
			binary.LittleEndian.PutUint32(rec[valOff:], kv[s])
		}
		buf = append(buf, rec...)
	}
	return buf
}

func flatIDList(ids []uint32) []byte {
	buf := make([]byte, 0, len(ids)*4)
	var w [4]byte
	for _, id := range ids {
		binary.LittleEndian.PutUint32(w[:], id)
		buf = append(buf, w[:]...)
	}
	return buf
}

func cardDBRows(rng *rand.Rand, ids []uint32) []byte {
	const stride = 48
	buf := make([]byte, 0, len(ids)*stride)
	for _, id := range ids {
		rec := make([]byte, stride)
		for i := range rec {
			rec[i] = byte(rng.Intn(256))
		}
		binary.LittleEndian.PutUint32(rec[0:], id)
		binary.LittleEndian.PutUint32(rec[12:], uint32(rng.Intn(900000)+1000)) // not a count
		buf = append(buf, rec...)
	}
	return buf
}

func sample(rng *rand.Rand, pool []uint32, n int) []uint32 {
	if n > len(pool) {
		n = len(pool)
	}
	p := rng.Perm(len(pool))
	out := make([]uint32, n)
	for i := 0; i < n; i++ {
		out[i] = pool[p[i]]
	}
	return out
}

// Build writes dir/fake.core and returns the planted ground truth.
func Build(dir string, s Spec) (Built, error) {
	rng := rand.New(rand.NewSource(s.Seed))
	absent := map[uint32]bool{}
	for _, a := range s.Absent {
		absent[a] = true
	}

	truth := map[uint32]uint32{}
	for k, v := range s.Owned {
		truth[k] = v
	}
	extraN := 1800
	if extraN > len(s.Valid) {
		extraN = len(s.Valid) / 2
	}
	for _, g := range sample(rng, s.Valid, extraN) {
		if _, ok := truth[g]; ok || absent[g] {
			continue
		}
		truth[g] = []uint32{1, 1, 2, 2, 3, 4}[rng.Intn(6)]
	}
	basics := map[uint32]bool{}
	keys := make([]uint32, 0, len(truth))
	for k := range truth {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for i := 0; i < 9 && i < len(keys); i++ {
		truth[keys[i]] = uint32(rng.Intn(35) + 5)
		basics[keys[i]] = true
	}

	precon := map[uint32]uint32{}
	for _, g := range sample(rng, s.Valid, min(2274, len(s.Valid))) {
		precon[g] = uint32(rng.Intn(4) + 1)
	}
	decoyAbsent := map[uint32]uint32{}
	for _, a := range s.Absent {
		decoyAbsent[a] = 2
	}
	for _, g := range sample(rng, s.Valid, min(900, len(s.Valid))) {
		decoyAbsent[g] = uint32(rng.Intn(4) + 1)
	}

	type payload struct {
		name string
		data []byte
	}
	payloads := []payload{
		{"collection", dictEntries(rng, truth, 16, 8, 12)},
		{"card_db", cardDBRows(rng, s.Valid)},
		{"precon", dictEntries(rng, precon, 24, 0, 4)},
		{"craftable", flatIDList(sample(rng, s.Valid, min(400, len(s.Valid))))},
		{"precon_with_absent", dictEntries(rng, decoyAbsent, 16, 8, 12)},
	}

	// Assemble file: header + N segment commands + N blobs.
	ncmds := len(payloads)
	fileoff := uint64(32 + 72*ncmds)
	var body []byte
	var segCmds []byte
	vm := uint64(segBase)
	for _, p := range payloads {
		padLen := (rng.Intn(60000) + 4096) &^ 0xF
		blob := make([]byte, padLen)
		for i := range blob {
			blob[i] = byte(rng.Intn(256))
		}
		blob = append(blob, p.data...)
		tail := make([]byte, 4096)
		blob = append(blob, tail...)
		if pad := (-len(blob)) & 0xF; pad != 0 {
			blob = append(blob, make([]byte, pad)...)
		}
		seg := make([]byte, 72)
		binary.LittleEndian.PutUint32(seg[0:], lcSegment64)
		binary.LittleEndian.PutUint32(seg[4:], 72)
		copy(seg[8:24], p.name)
		binary.LittleEndian.PutUint64(seg[24:], vm)
		binary.LittleEndian.PutUint64(seg[32:], uint64(len(blob)))
		binary.LittleEndian.PutUint64(seg[40:], fileoff)
		binary.LittleEndian.PutUint64(seg[48:], uint64(len(blob)))
		binary.LittleEndian.PutUint32(seg[56:], 0x7)
		binary.LittleEndian.PutUint32(seg[60:], 0x3)
		segCmds = append(segCmds, seg...)
		body = append(body, blob...)
		fileoff += uint64(len(blob))
		vm += segStride
	}

	hdr := make([]byte, 32)
	binary.LittleEndian.PutUint32(hdr[0:], 0xFEEDFACF)
	binary.LittleEndian.PutUint32(hdr[4:], 0x0100000C)
	binary.LittleEndian.PutUint32(hdr[12:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], uint32(ncmds))
	binary.LittleEndian.PutUint32(hdr[20:], uint32(72*ncmds))

	path := filepath.Join(dir, "fake.core")
	f, err := os.Create(path)
	if err != nil {
		return Built{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(append(hdr, segCmds...), body...)); err != nil {
		return Built{}, err
	}
	return Built{CorePath: path, Truth: truth, Basics: basics}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/testcore/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/testcore
git commit --no-gpg-sign -m "test(testcore): synthetic core builder with planted collection and decoys"
```

---

## Task 4: `scan` — find candidate collection tables

Port of `collect_hits` / `clusters` / `residue_runs` and candidate assembly from `scan_core.py`. Scan is assumption-free: it produces every plausible `{grpId->count}` table; the oracle judges them.

**Files:**
- Create: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `*machocore.Core` (Task 2), `testcore.Build` (Task 3, test only).
- Produces:
  - `type Candidate struct { Base uint64; Stride, CountOffset int; Entries map[uint32]uint32 }`
  - `func FindCandidates(c *machocore.Core, valid map[uint32]bool, align int) []Candidate`

Algorithm (handoff §5, §7):
1. For each writable segment, scan 4-byte LE words at the given `align` (4 default; 1 fallback shifts). Keep addr→grpId for words in `valid`.
2. Cluster all sighting addresses by byte-proximity, gap ≤ 512, dropping clusters < 60 hits (§7.4 — cluster tables, not hits).
3. Within a cluster, for each stride in `STRIDES`, bucket addresses by `addr % stride`; each residue with ≥ 60 members is a run.
4. For each run and each `COUNT_OFFSETS` delta, read the count word at `addr+delta`; build `Entries` from members whose count is in `1..64` (keeps basics; drops zero/garbage). Keep the candidate if it has ≥ 60 entries.

- [ ] **Step 1: Write the failing test**

`internal/scan/scan_test.go`:

```go
package scan

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/machocore"
	"github.com/bynov/mtga-collection-export/internal/testcore"
)

func TestFindCandidatesRecoversPlantedCollection(t *testing.T) {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	b, err := testcore.Build(t.TempDir(), testcore.Spec{
		Valid:  valid,
		Owned:  map[uint32]uint32{7000: 4, 7010: 2, 7020: 1},
		Absent: []uint32{9999999},
		Seed:   1337,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := machocore.Open(b.CorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	lut := map[uint32]bool{}
	for _, id := range valid {
		lut[id] = true
	}
	cands := FindCandidates(c, lut, 4)
	if len(cands) == 0 {
		t.Fatal("no candidates found")
	}

	// At least one candidate must reproduce the planted collection exactly
	// (stride 16, count at +12 for the collection table).
	var matched *Candidate
	for i := range cands {
		exact := len(cands[i].Entries) == len(b.Truth)
		if !exact {
			continue
		}
		ok := true
		for g, n := range b.Truth {
			if cands[i].Entries[g] != n {
				ok = false
				break
			}
		}
		if ok {
			matched = &cands[i]
			break
		}
	}
	if matched == nil {
		t.Fatal("no candidate reproduced the planted collection exactly")
	}
	if matched.Stride != 16 || matched.CountOffset != 12 {
		t.Fatalf("recovered shape stride=%d count@%+d, want stride=16 count@+12", matched.Stride, matched.CountOffset)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/scan/`
Expected: FAIL — `undefined: FindCandidates`.

- [ ] **Step 3: Implement**

`internal/scan/scan.go`:

```go
// Package scan locates candidate collection tables in a core dump without
// assuming their layout. The oracle layer validates the candidates it returns.
package scan

import (
	"encoding/binary"
	"sort"

	"github.com/bynov/mtga-collection-export/internal/machocore"
)

var strides = []int{8, 12, 16, 20, 24, 28, 32, 40, 48, 64}
var countOffsets = []int{-16, -12, -8, -4, 4, 8, 12, 16}

const (
	maxGap  = 512
	minHits = 60
)

// Candidate is one hypothesised table: a run at (Base, Stride) with the count
// field at Base+CountOffset, and the grpId->count map it yields.
type Candidate struct {
	Base        uint64
	Stride      int
	CountOffset int
	Entries     map[uint32]uint32
}

// collectHits maps every valid-grpId sighting address to its grpId.
func collectHits(c *machocore.Core, valid map[uint32]bool, align int) map[uint64]uint32 {
	hits := map[uint64]uint32{}
	shifts := []int{0}
	if align < 4 {
		shifts = []int{0, 1, 2, 3}
		align = 1
	}
	for _, s := range c.Segments() {
		buf, err := c.SegmentBytes(s)
		if err != nil {
			continue
		}
		for _, sh := range shifts {
			for off := sh; off+4 <= len(buf); off += align {
				v := binary.LittleEndian.Uint32(buf[off:])
				if valid[v] {
					hits[s.VMAddr+uint64(off)] = v
				}
			}
		}
	}
	return hits
}

// clusters splits sorted sighting addresses into byte-proximity groups.
func clusters(addrs []uint64) [][]uint64 {
	if len(addrs) == 0 {
		return nil
	}
	var out [][]uint64
	cur := []uint64{addrs[0]}
	for _, a := range addrs[1:] {
		if a-cur[len(cur)-1] <= maxGap {
			cur = append(cur, a)
		} else {
			if len(cur) >= minHits {
				out = append(out, cur)
			}
			cur = []uint64{a}
		}
	}
	if len(cur) >= minHits {
		out = append(out, cur)
	}
	return out
}

// residueRuns groups a cluster's addresses by addr % stride.
func residueRuns(cluster []uint64, stride int) [][]uint64 {
	byRes := map[uint64][]uint64{}
	for _, a := range cluster {
		byRes[a%uint64(stride)] = append(byRes[a%uint64(stride)], a)
	}
	var out [][]uint64
	for _, g := range byRes {
		if len(g) >= minHits {
			sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
			out = append(out, g)
		}
	}
	return out
}

// FindCandidates enumerates every (stride, count-offset) hypothesis with enough
// entries. align=4 scans the word grid; align<4 falls back to unaligned.
func FindCandidates(c *machocore.Core, valid map[uint32]bool, align int) []Candidate {
	hits := collectHits(c, valid, align)
	if len(hits) == 0 {
		return nil
	}
	addrs := make([]uint64, 0, len(hits))
	for a := range hits {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })

	var out []Candidate
	for _, cl := range clusters(addrs) {
		for _, stride := range strides {
			for _, run := range residueRuns(cl, stride) {
				for _, delta := range countOffsets {
					entries := make(map[uint32]uint32, len(run))
					for _, a := range run {
						cnt, ok := c.ReadU32(uint64(int64(a) + int64(delta)))
						if !ok {
							continue
						}
						if cnt >= 1 && cnt <= 64 {
							entries[hits[a]] = cnt
						}
					}
					if len(entries) >= minHits {
						out = append(out, Candidate{Base: run[0], Stride: stride, CountOffset: delta, Entries: entries})
					}
				}
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scan
git commit --no-gpg-sign -m "feat(scan): recover candidate collection tables from a core"
```

---

## Task 5: `starthook` — parse played decks + non-collectible list

The StartHook JSON bootstraps the oracle (handoff §3.2): decks with a real `LastPlayed` prove ownership; `CardMetadataInfo.NonCollectibleCardList` is a structural filter. Precon (`?=?Loc/Decks/Precon/` name prefix) and imported/`0001-01-01` decks are excluded.

**Implementation note (must verify against a real capture):** the exact JSON field for a deck's card list is not documented in the handoff. The struct below uses `json.RawMessage` for the deck body and a tolerant extractor that pulls every `{grpId, count}`-shaped pair. **Before implementing, capture one real StartHook** (`acquire.ExtractStartHook`, Task 9, or `awk 'NR==<line>' Player.log`) and confirm the field names; adjust the `deckCards` extractor to match. The structural oracle tests (Task 6) are the safety net if bootstrap under-derives.

**Files:**
- Create: `internal/starthook/starthook.go`
- Test: `internal/starthook/starthook_test.go`

**Interfaces:**
- Produces:
  - `type StartHook struct { ... }` (unexported fields)
  - `func Parse(raw []byte) (*StartHook, error)`
  - `func (s *StartHook) NonCollectible() map[uint32]bool`
  - `func (s *StartHook) PlayedDeckCards() map[uint32]uint32` — grpId → max copies across played decks (a deck running 4× proves ≥4)

- [ ] **Step 1: Write the failing test**

`internal/starthook/starthook_test.go`:

```go
package starthook

import "testing"

// Minimal StartHook fixture: one played deck, one precon, one imported.
const fixture = `{
  "CardMetadataInfo": {"NonCollectibleCardList": [100, 200]},
  "Decks": [
    {"Name": "My Mono Red",
     "Attributes": [{"Name": "LastPlayed", "Value": "2026-07-20T10:00:00"}],
     "MainDeck": [{"cardId": 7000, "quantity": 4}, {"cardId": 7010, "quantity": 2}]},
    {"Name": "?=?Loc/Decks/Precon/RNA_Azorius",
     "Attributes": [{"Name": "LastPlayed", "Value": "2026-01-01T00:00:00"}],
     "MainDeck": [{"cardId": 500, "quantity": 4}]},
    {"Name": "Imported Deck",
     "Attributes": [{"Name": "LastPlayed", "Value": "0001-01-01T00:00:00"}],
     "MainDeck": [{"cardId": 87145, "quantity": 4}]}
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
		t.Fatal("imported-deck card leaked into owned")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/starthook/`
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 3: Implement**

`internal/starthook/starthook.go`:

```go
// Package starthook parses Arena's StartHook JSON to bootstrap the oracle:
// proven-owned cards from played decks and the non-collectible filter list.
package starthook

import (
	"encoding/json"
	"strings"
)

type rawHook struct {
	CardMetadataInfo struct {
		NonCollectibleCardList []uint32 `json:"NonCollectibleCardList"`
	} `json:"CardMetadataInfo"`
	Decks []rawDeck `json:"Decks"`
}

type rawDeck struct {
	Name       string `json:"Name"`
	Attributes []struct {
		Name  string `json:"Name"`
		Value string `json:"Value"`
	} `json:"Attributes"`
	MainDeck []struct {
		CardID   uint32 `json:"cardId"`
		Quantity uint32 `json:"quantity"`
	} `json:"MainDeck"`
}

// StartHook is the parsed payload.
type StartHook struct{ raw rawHook }

// Parse decodes a single StartHook JSON line.
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

// isPlayed reports whether a deck has a real LastPlayed date and is not a precon.
func isPlayed(d rawDeck) bool {
	if strings.HasPrefix(d.Name, "?=?Loc/Decks/Precon/") {
		return false
	}
	for _, a := range d.Attributes {
		if a.Name != "LastPlayed" {
			continue
		}
		v := a.Value
		if v == "" || strings.HasPrefix(v, "0001-01-01") {
			return false
		}
		return true
	}
	return false
}

// PlayedDeckCards returns grpId -> max copies across played decks.
func (s *StartHook) PlayedDeckCards() map[uint32]uint32 {
	out := map[uint32]uint32{}
	for _, d := range s.raw.Decks {
		if !isPlayed(d) {
			continue
		}
		for _, card := range d.MainDeck {
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
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/starthook/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/starthook
git commit --no-gpg-sign -m "feat(starthook): bootstrap oracle from played decks and non-collectible list"
```

---

## Task 6: `oracle` — validate candidates, reconcile variants

Port of `score` + `validate.py`. The oracle gates all output (handoff §5, §9). Structural tests: 100% of proven-owned present, zero known-absent, no non-collectible ids, no non-basic count > 4, count ≥ played (excluding basics). Winner = every candidate passing all tests; final counts = **element-wise max** across them (handoff §7.8 — ownership is monotonic).

**Files:**
- Create: `internal/oracle/oracle.go`
- Test: `internal/oracle/oracle_test.go`

**Interfaces:**
- Consumes: `scan.Candidate` (Task 4).
- Produces:
  - `type Oracle struct { Valid, NonCollectible, Basics map[uint32]bool; KnownOwned map[uint32]uint32; KnownAbsent map[uint32]bool }`
  - `type Result struct { Counts map[uint32]uint32; Variants int; Degraded bool; Evidence string }`
  - `func (o *Oracle) Evaluate(cands []scan.Candidate) (*Result, error)` — returns an error whose message is the evidence table when nothing passes.

Degraded mode: if `KnownOwned` is empty (no played decks), skip the presence test and set `Degraded=true`; structural tests still apply. The caller prints the degraded warning.

- [ ] **Step 1: Write the failing test**

`internal/oracle/oracle_test.go`:

```go
package oracle

import (
	"testing"

	"github.com/bynov/mtga-collection-export/internal/scan"
)

func base() *Oracle {
	return &Oracle{
		Valid:          map[uint32]bool{7000: true, 7010: true, 7020: true, 8000: true},
		NonCollectible: map[uint32]bool{999: true},
		Basics:         map[uint32]bool{7020: true},
		KnownOwned:     map[uint32]uint32{7000: 4, 7010: 2},
		KnownAbsent:    map[uint32]bool{9999999: true},
	}
}

func TestEvaluateSelectsAndReconcilesMax(t *testing.T) {
	// Two passing variants that disagree by 1 on 7010: max must win.
	v0 := scan.Candidate{Stride: 8, CountOffset: 4, Entries: map[uint32]uint32{7000: 4, 7010: 2, 7020: 20}}
	v1 := scan.Candidate{Stride: 8, CountOffset: 4, Entries: map[uint32]uint32{7000: 4, 7010: 1, 7020: 20}}
	res, err := base().Evaluate([]scan.Candidate{v1, v0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[7010] != 2 {
		t.Fatalf("reconcile should take max: 7010 = %d, want 2", res.Counts[7010])
	}
	if res.Variants != 2 {
		t.Fatalf("Variants = %d, want 2", res.Variants)
	}
}

func TestEvaluateRejectsForbiddenAndOverCap(t *testing.T) {
	o := base()
	// Contains a known-absent id.
	bad1 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 9999999: 2}}
	// Non-basic over the cap of 4.
	bad2 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 8000: 7}}
	// Contains a non-collectible id.
	bad3 := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 999: 1}}
	// Missing a proven-owned card (7010).
	bad4 := scan.Candidate{Entries: map[uint32]uint32{7000: 4}}
	if _, err := o.Evaluate([]scan.Candidate{bad1, bad2, bad3, bad4}); err == nil {
		t.Fatal("expected rejection of all-bad candidate set")
	}
}

func TestBasicLandBelowPlayedCountIsAllowed(t *testing.T) {
	o := base()
	o.KnownOwned[7020] = 24 // a deck ran 24 of a basic land
	// Stored as 1 for a basic — must NOT count as a violation.
	good := scan.Candidate{Entries: map[uint32]uint32{7000: 4, 7010: 2, 7020: 1}}
	if _, err := o.Evaluate([]scan.Candidate{good}); err != nil {
		t.Fatalf("basic land below played count should pass: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/oracle/`
Expected: FAIL — `undefined: Oracle`.

- [ ] **Step 3: Implement**

`internal/oracle/oracle.go`:

```go
// Package oracle validates candidate collection tables against facts we trust
// and reconciles the survivors. It never returns an unvalidated result.
package oracle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bynov/mtga-collection-export/internal/scan"
)

// Oracle holds the ground-truth facts used to judge candidates.
type Oracle struct {
	Valid          map[uint32]bool
	NonCollectible map[uint32]bool
	Basics         map[uint32]bool
	KnownOwned     map[uint32]uint32 // grpId -> copies proven by played decks
	KnownAbsent    map[uint32]bool
}

// Result is a validated collection.
type Result struct {
	Counts   map[uint32]uint32
	Variants int
	Degraded bool
	Evidence string
}

type verdict struct {
	cand                   scan.Candidate
	missing, forbidden     int
	nonCollectible, overCap int
	qtyViolations          int
}

func (o *Oracle) judge(c scan.Candidate) verdict {
	v := verdict{cand: c}
	for g := range o.KnownOwned {
		if _, ok := c.Entries[g]; !ok {
			v.missing++
		}
	}
	for g, cnt := range c.Entries {
		if o.KnownAbsent[g] {
			v.forbidden++
		}
		if o.NonCollectible[g] {
			v.nonCollectible++
		}
		if cnt > 4 && !o.Basics[g] {
			v.overCap++
		}
		if want, ok := o.KnownOwned[g]; ok && cnt < want && !o.Basics[g] {
			v.qtyViolations++
		}
	}
	return v
}

func (v verdict) passes() bool {
	return v.missing == 0 && v.forbidden == 0 && v.nonCollectible == 0 && v.overCap == 0 && v.qtyViolations == 0
}

// Evaluate selects passing candidates and returns their element-wise max.
func (o *Oracle) Evaluate(cands []scan.Candidate) (*Result, error) {
	degraded := len(o.KnownOwned) == 0
	var passing []scan.Candidate
	var rejects []verdict
	for _, c := range cands {
		v := o.judge(c)
		if v.passes() {
			passing = append(passing, c)
		} else {
			rejects = append(rejects, v)
		}
	}
	if len(passing) == 0 {
		return nil, fmt.Errorf("no candidate passed validation\n%s", evidence(rejects))
	}
	counts := map[uint32]uint32{}
	for _, c := range passing {
		for g, n := range c.Entries {
			if n > counts[g] {
				counts[g] = n
			}
		}
	}
	return &Result{Counts: counts, Variants: len(passing), Degraded: degraded, Evidence: passSummary(passing)}, nil
}

func passSummary(passing []scan.Candidate) string {
	sizes := make([]int, len(passing))
	for i, c := range passing {
		sizes[i] = len(c.Entries)
	}
	sort.Ints(sizes)
	return fmt.Sprintf("%d passing variants, entry counts %v", len(passing), sizes)
}

func evidence(rejects []verdict) string {
	sort.Slice(rejects, func(i, j int) bool { return len(rejects[i].cand.Entries) > len(rejects[j].cand.Entries) })
	var b strings.Builder
	fmt.Fprintf(&b, "%6s %6s %8s %8s %8s %8s %8s\n", "stride", "cnt@", "cards", "missing", "absent", "noncol", ">4")
	for i, v := range rejects {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "%6d %+6d %8d %8d %8d %8d %8d\n",
			v.cand.Stride, v.cand.CountOffset, len(v.cand.Entries), v.missing, v.forbidden, v.nonCollectible, v.overCap)
	}
	return b.String()
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/oracle/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/oracle
git commit --no-gpg-sign -m "feat(oracle): validate candidates and reconcile variants by element-wise max"
```

---

## Task 7: `carddb` — resolve grpId to card details

Port of `mtga_export.py`'s SQLite join plus the handoff §6 gotchas: rarity int map, `OldSchoolManaText` shorthand, enum resolution, markup stripping, `IsRebalanced=1` filter, collectible filter. The client DB is opened read-only on a copy.

**Files:**
- Create: `internal/carddb/carddb.go`
- Test: `internal/carddb/carddb_test.go`

**Interfaces:**
- Produces:
  - `type Card struct { GrpID uint32; Name, Set, CollectorNumber, Rarity, Colors, TypeLine, ManaCost string; IsBasicLand bool }`
  - `type DB struct { ... }`
  - `func Open(path string) (*DB, error)` — opens `file:<path>?mode=ro` via `modernc.org/sqlite`
  - `func (db *DB) Resolve(grpID uint32) (Card, bool)`
  - `func (db *DB) Rebalanced() map[uint32]bool`
  - `func (db *DB) Basics() map[uint32]bool`
  - `func (db *DB) Collectible() map[uint32]bool`
  - `func (db *DB) Close() error`

The test builds a tiny synthetic SQLite DB with the same schema shape (`Cards`, `Localizations_enUS`, `Enums`) so no real 249 MB DB is needed.

- [ ] **Step 1: Write the failing test**

`internal/carddb/carddb_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/carddb/`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Implement**

`internal/carddb/carddb.go`:

```go
// Package carddb resolves Arena grpIds to card details from the client's local
// SQLite card database, opened read-only on a copy.
package carddb

import (
	"database/sql"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"
)

var rarityName = map[int]string{0: "", 1: "Basic Land", 2: "Common", 3: "Uncommon", 4: "Rare", 5: "Mythic"}
var colorLetter = map[string]string{"White": "W", "Blue": "U", "Black": "B", "Red": "R", "Green": "G"}
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
	db                   *sql.DB
	cardType, subType    map[int]string
	superType, cardColor map[int]string
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
		{"SuperType", &d.superType}, {"CardColor", &d.cardColor},
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
		n := 0
		for _, c := range m {
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
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
		col.WriteString(colorLetter[d.cardColor[id]])
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
```

- [ ] **Step 4: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./internal/carddb/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/carddb
git commit --no-gpg-sign -m "feat(carddb): resolve grpIds to cards, filter rebalanced/tokens"
```

---

## Task 8: `export` — write CSV and report totals

Writes CSV in three formats and returns the printings/distinct-names/copies report (handoff §6). The rebalanced filter is applied by the caller (Task 10) before export; export just writes what it's given and counts.

**Files:**
- Create: `internal/export/export.go`
- Test: `internal/export/export_test.go`

**Interfaces:**
- Consumes: `carddb.Card` (Task 7).
- Produces:
  - `type Row struct { Count uint32; Card carddb.Card }`
  - `type Report struct { Printings, DistinctNames int; Copies uint32 }`
  - `func Write(path, format string, rows []Row) (Report, error)` — `format` ∈ {`raw`,`moxfield`,`manabox`}; rows sorted by name then set; returns error on unknown format.

Formats:
- `raw`: `count,name,set,collector_number,rarity,colors,type,mana_cost,arena_id`
- `moxfield`: `Count,Name,Edition,Collector Number` (Moxfield CSV import columns)
- `manabox`: `Name,Set code,Collector number,Quantity`

- [ ] **Step 1: Write the failing test**

`internal/export/export_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/export/`
Expected: FAIL — `undefined: Write`.

- [ ] **Step 3: Implement**

`internal/export/export.go`:

```go
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
	switch format {
	case "raw":
		return []string{"count", "name", "set", "collector_number", "rarity", "colors", "type", "mana_cost", "arena_id"},
			func(r Row) []string {
				return []string{
					strconv.FormatUint(uint64(r.Count), 10), r.Card.Name, r.Card.Set,
					r.Card.CollectorNumber, r.Card.Rarity, r.Card.Colors, r.Card.TypeLine,
					r.Card.ManaCost, strconv.FormatUint(uint64(r.Card.GrpID), 10),
				}
			}, nil
	case "moxfield":
		return []string{"Count", "Name", "Edition", "Collector Number"},
			func(r Row) []string {
				return []string{strconv.FormatUint(uint64(r.Count), 10), r.Card.Name, r.Card.Set, r.Card.CollectorNumber}
			}, nil
	case "manabox":
		return []string{"Name", "Set code", "Collector number", "Quantity"},
			func(r Row) []string {
				return []string{r.Card.Name, r.Card.Set, r.Card.CollectorNumber, strconv.FormatUint(uint64(r.Count), 10)}
			}, nil
	default:
		return nil, nil, fmt.Errorf("unknown format %q (want raw, moxfield, or manabox)", format)
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/export/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/export
git commit --no-gpg-sign -m "feat(export): CSV writers and printings/names/copies report"
```

---

## Task 9: `acquire` — locate files and dump the core (macOS)

The only OS-specific layer. Locates the running MTGA pid and data files, copies the card DB (the client holds it open), extracts the StartHook line from Player.log, and dumps a core via lldb. Command construction and StartHook extraction are unit-tested; the live `lldb`/`pgrep` calls are exercised only by the optional manual path in Task 11.

**Files:**
- Create: `internal/acquire/acquire.go`
- Test: `internal/acquire/acquire_test.go`

**Interfaces:**
- Produces:
  - `func FindCardDB() (string, error)` — newest `Raw_CardDatabase_*.mtga` under `~/Library/Application Support/com.wizards.mtga/Downloads/Raw/`
  - `func CopyCardDB(src, dstDir string) (string, error)` — copies to `dstDir/carddb.sqlite`
  - `func PlayerLogPath() string` — `~/Library/Logs/Wizards Of The Coast/MTGA/Player.log`
  - `func ExtractStartHook(logPath string) ([]byte, error)` — the last `<== StartHook` JSON object in the log
  - `func FindPID() (int, error)` — `pgrep -f 'MacOS/MTGA'`, first match
  - `func lldbArgs(pid int, outPath string) []string` — pure, testable command builder
  - `func DumpCore(pid int, outPath string) error` — runs lldb, verifies output exists

Handoff §4: `lldb -p <pid> -b -o "process save-core --style full <out>" -o detach -o quit`. Use `--style full` (§7.8 — modified-memory returns stale variants).

- [ ] **Step 1: Write the failing test**

`internal/acquire/acquire_test.go`:

```go
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
	content := strings.Join([]string{
		`[UnityCrossThreadLogger]==> StartHook {"junk":1}`,
		`[UnityCrossThreadLogger]<== StartHook {"InventoryInfo":{"a":1},"Decks":[]}`,
		`some other line`,
		`[UnityCrossThreadLogger]<== StartHook {"InventoryInfo":{"a":2},"Decks":[{"Name":"x"}]}`,
	}, "\n")
	os.WriteFile(log, []byte(content), 0o644)

	got, err := ExtractStartHook(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"a":2`) {
		t.Fatalf("should return the LAST StartHook: %s", got)
	}
	if strings.Contains(string(got), "<== StartHook") {
		t.Fatalf("prefix not stripped: %s", got)
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/acquire/`
Expected: FAIL — `undefined: lldbArgs`.

- [ ] **Step 3: Implement**

`internal/acquire/acquire.go`:

```go
// Package acquire is the macOS-specific layer: it locates the running MTGA
// process and its data files, and snapshots memory via lldb. Read-only.
package acquire

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const startHookMarker = "<== StartHook "

// FindCardDB returns the newest Raw_CardDatabase_*.mtga path.
func FindCardDB() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Application Support", "com.wizards.mtga", "Downloads", "Raw")
	matches, err := filepath.Glob(filepath.Join(dir, "Raw_CardDatabase_*.mtga"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("no Raw_CardDatabase_*.mtga under %s (is Arena installed?)", dir)
	}
	sort.Slice(matches, func(i, j int) bool {
		fi, _ := os.Stat(matches[i])
		fj, _ := os.Stat(matches[j])
		return fi.ModTime().After(fj.ModTime())
	})
	return matches[0], nil
}

// CopyCardDB copies the card DB (the client holds it open) to dstDir/carddb.sqlite.
func CopyCardDB(src, dstDir string) (string, error) {
	dst := filepath.Join(dstDir, "carddb.sqlite")
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return dst, nil
}

// PlayerLogPath returns the standard Player.log location.
func PlayerLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "Wizards Of The Coast", "MTGA", "Player.log")
}

// ExtractStartHook returns the JSON of the last `<== StartHook` line.
func ExtractStartHook(logPath string) ([]byte, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // StartHook is ~210 KB
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, startHookMarker); i >= 0 {
			last = line[i+len(startHookMarker):]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if last == "" {
		return nil, fmt.Errorf("no StartHook line in %s (enable Detailed Logs and restart Arena)", logPath)
	}
	return []byte(strings.TrimSpace(last)), nil
}

// FindPID returns the MTGA process id.
func FindPID() (int, error) {
	out, err := exec.Command("pgrep", "-f", "MacOS/MTGA").Output()
	if err != nil {
		return 0, fmt.Errorf("MTGA is not running (pgrep: %w)", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("MTGA is not running")
	}
	return strconv.Atoi(fields[0])
}

func lldbArgs(pid int, outPath string) []string {
	return []string{
		"-p", strconv.Itoa(pid), "-b",
		"-o", fmt.Sprintf("process save-core --style full %q", outPath),
		"-o", "detach", "-o", "quit",
	}
}

// DumpCore snapshots the process to outPath. Arena is paused only during the write.
func DumpCore(pid int, outPath string) error {
	cmd := exec.Command("lldb", lldbArgs(pid, outPath)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lldb save-core failed: %w (is Xcode Command Line Tools installed?)", err)
	}
	fi, err := os.Stat(outPath)
	if err != nil || fi.Size() == 0 {
		return fmt.Errorf("lldb produced no core at %s", outPath)
	}
	return nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/acquire/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/acquire
git commit --no-gpg-sign -m "feat(acquire): locate MTGA files and dump core via lldb"
```

---

## Task 10: `cmd/mtga-collection-export` — CLI orchestration

Wires the layers: locate → dump → parse → bootstrap oracle → scan → validate → reconcile → resolve → filter rebalanced → export. Prints the verdict and the three-figure report. On validation failure, prints evidence and exits non-zero. Supports `--core` to skip acquisition (reproducible testing).

**Files:**
- Create: `cmd/mtga-collection-export/main.go`
- Create: `cmd/mtga-collection-export/run.go` (the orchestration function, testable with `--core`)
- Test: `cmd/mtga-collection-export/run_test.go`

**Interfaces:**
- Consumes: all packages above.
- Produces: `func run(opts Options) error` and `type Options struct { Core, Out, Format, CardDB, StartHookPath string; Absent []uint32; KeepCore, Verbose bool }`.

Flags (spec §5): `--out` (default `mtga_collection.csv`), `--format` (raw|moxfield|manabox), `--absent`, `--keep-core`, `--core`, `--carddb`, `--starthook`, `--verbose`.

Orchestration for the `--core` path (used by the test — no game required):
1. Build oracle: `valid = carddb.Collectible()`, `basics = carddb.Basics()`; if StartHook given, `nonCollectible` + `knownOwned` from it; add `--absent` to `KnownAbsent`.
2. `cands := scan.FindCandidates(core, valid, 4)`; if `oracle.Evaluate` errors, retry with `align=1`.
3. On success: drop rebalanced grpIds (`carddb.Rebalanced`), resolve each grpId, build rows, `export.Write`.
4. Print report: printings / distinct names / copies; note variants reconciled; list dropped rebalanced/unresolved counts; if `Degraded`, print the degraded-oracle warning.

- [ ] **Step 1: Write the failing end-to-end test**

`cmd/mtga-collection-export/run_test.go`:

```go
package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/bynov/mtga-collection-export/internal/testcore"
	_ "modernc.org/sqlite"
)

// buildCardDB writes a card DB covering the planted valid ids so resolution and
// the collectible filter work end-to-end.
func buildCardDB(t *testing.T, valid []uint32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "carddb.sqlite")
	db, _ := sql.Open("sqlite", path)
	defer db.Close()
	db.Exec(`CREATE TABLE Cards (GrpId INTEGER, TitleId INTEGER, ExpansionCode TEXT,
		CollectorNumber TEXT, Rarity INTEGER, Colors TEXT, Types TEXT, Subtypes TEXT,
		Supertypes TEXT, OldSchoolManaText TEXT, IsPrimaryCard INTEGER, IsToken INTEGER, IsRebalanced INTEGER)`)
	db.Exec(`CREATE TABLE Localizations_enUS (LocId INTEGER, Loc TEXT, Formatted INTEGER)`)
	db.Exec(`CREATE TABLE Enums (Type TEXT, Value INTEGER, LocId INTEGER)`)
	tx, _ := db.Begin()
	st, _ := tx.Prepare(`INSERT INTO Cards VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	ln, _ := tx.Prepare(`INSERT INTO Localizations_enUS VALUES (?,?,1)`)
	for i, g := range valid {
		st.Exec(g, i+1, "TST", "1", 2, "1", "1", "", "", "o1", 1, 0, 0)
		ln.Exec(i+1, "Card "+string(rune('A'+i%26)), )
	}
	tx.Commit()
	return path
}

func TestRunEndToEndOnSyntheticCore(t *testing.T) {
	valid := make([]uint32, 0, 3000)
	for id := uint32(6873); id < 6873+3000; id++ {
		valid = append(valid, id)
	}
	b, err := testcore.Build(t.TempDir(), testcore.Spec{
		Valid:  valid,
		Owned:  map[uint32]uint32{7000: 4, 7010: 2, 7020: 1},
		Absent: []uint32{9999999},
		Seed:   1337,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "collection.csv")
	err = run(Options{
		Core:   b.CorePath,
		Out:    out,
		Format: "raw",
		CardDB: buildCardDB(t, valid),
		// no StartHook -> degraded mode; structural tests still gate output
		Absent: []uint32{9999999},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatal("no CSV written")
	}
}

func TestRunRefusesWhenNothingValidates(t *testing.T) {
	// A core with no plausible collection: empty valid set => no candidates.
	valid := []uint32{6873}
	b, _ := testcore.Build(t.TempDir(), testcore.Spec{Valid: valid, Owned: map[uint32]uint32{}, Seed: 1})
	err := run(Options{
		Core:   b.CorePath,
		Out:    filepath.Join(t.TempDir(), "x.csv"),
		Format: "raw",
		CardDB: buildCardDB(t, valid),
	})
	if err == nil {
		t.Fatal("run must refuse to emit when nothing validates")
	}
}
```

Note: the degraded-mode end-to-end test relies on the structural tests alone. If the synthetic decoys pass structural tests without an oracle (possible, since decoys avoid `>4` and non-collectible), tighten the test to pass a StartHook fixture instead. Prefer adding a minimal StartHook (`--starthook`) so the presence test is exercised; adjust `run` to accept `StartHookPath` and the test to write a StartHook JSON whose played deck lists grpIds 7000/7010/7020.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/mtga-collection-export/`
Expected: FAIL — `undefined: run`.

- [ ] **Step 3: Implement `run.go`**

`cmd/mtga-collection-export/run.go`:

```go
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

	result, err := validate(core, orc)
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

// validate scans at align 4, falls back to align 1 if nothing passes.
func validate(core *machocore.Core, orc *oracle.Oracle) (*oracle.Result, error) {
	for _, align := range []int{4, 1} {
		cands := scan.FindCandidates(core, orc.Valid, align)
		if res, err := orc.Evaluate(cands); err == nil {
			return res, nil
		}
	}
	cands := scan.FindCandidates(core, orc.Valid, 4)
	_, err := orc.Evaluate(cands)
	return nil, fmt.Errorf("no validated collection table found — was the Collection tab opened and scrolled before dumping?\n%w", err)
}
```

- [ ] **Step 4: Implement `main.go`**

`cmd/mtga-collection-export/main.go`:

```go
// Command mtga-collection-export dumps a macOS MTG Arena player's collection to CSV,
// read-only and offline. It refuses to emit output that fails validation.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bynov/mtga-collection-export/internal/acquire"
	"github.com/bynov/mtga-collection-export/internal/version"
)

func main() {
	var (
		out       = flag.String("out", "mtga_collection.csv", "output CSV path")
		format    = flag.String("format", "raw", "raw | moxfield | manabox")
		absentCSV = flag.String("absent", "", "comma-separated grpIds you do NOT own (hardens the oracle)")
		core      = flag.String("core", "", "scan an existing core instead of dumping (testing/repro)")
		cardDB    = flag.String("carddb", "", "override card DB path")
		startHook = flag.String("starthook", "", "override StartHook JSON path")
		keepCore  = flag.Bool("keep-core", false, "keep the lldb core dump")
		verbose   = flag.Bool("verbose", false, "print candidate scoring")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version.String)
		return
	}

	opts := Options{
		Core: *core, Out: *out, Format: *format, CardDB: *cardDB,
		StartHookPath: *startHook, KeepCore: *keepCore, Verbose: *verbose,
		Absent: parseAbsent(*absentCSV),
	}

	// Acquire when no --core given.
	if opts.Core == "" {
		if err := acquireInputs(&opts); err != nil {
			fatal(err)
		}
	} else if opts.CardDB == "" {
		fatal(fmt.Errorf("--carddb is required with --core"))
	}

	if err := run(opts); err != nil {
		fatal(err)
	}
}

func acquireInputs(opts *Options) error {
	tmp, err := os.MkdirTemp("", "mtga-collection-export-*")
	if err != nil {
		return err
	}
	if !opts.KeepCore {
		defer os.RemoveAll(tmp)
	}

	src, err := acquire.FindCardDB()
	if err != nil {
		return err
	}
	dbCopy, err := acquire.CopyCardDB(src, tmp)
	if err != nil {
		return err
	}
	opts.CardDB = dbCopy

	if sh, err := acquire.ExtractStartHook(acquire.PlayerLogPath()); err == nil {
		p := filepath.Join(tmp, "starthook.json")
		if os.WriteFile(p, sh, 0o644) == nil {
			opts.StartHookPath = p
		}
	} else {
		fmt.Fprintln(os.Stderr, "WARNING:", err)
	}

	pid, err := acquire.FindPID()
	if err != nil {
		return err
	}
	corePath := filepath.Join(tmp, "mtga.core")
	fmt.Fprintln(os.Stderr, "dumping Arena memory (pauses the game only while the snapshot is written)...")
	if err := acquire.DumpCore(pid, corePath); err != nil {
		return err
	}
	opts.Core = corePath
	return nil
}

func parseAbsent(s string) []uint32 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []uint32
	for _, tok := range strings.Split(s, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(tok), 10, 32); err == nil {
			out = append(out, uint32(n))
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
```

- [ ] **Step 5: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./cmd/mtga-collection-export/`
Expected: PASS (both end-to-end and refusal tests).

If `TestRunEndToEndOnSyntheticCore` fails because degraded structural tests admit a decoy, switch it to write a StartHook fixture and pass `StartHookPath` (see the note in Step 1).

- [ ] **Step 6: Commit**

```bash
git add cmd/mtga-collection-export
git commit --no-gpg-sign -m "feat(cmd): wire acquisition, scan, validation, and export"
```

---

## Task 11: Build, vet, and manual end-to-end verification

**Files:** none (verification task).

- [ ] **Step 1: Full build and vet, cgo-free**

Run:
```bash
CGO_ENABLED=0 go build ./...
go vet ./...
CGO_ENABLED=0 go test ./...
```
Expected: all pass; a binary builds with cgo disabled.

- [ ] **Step 2: Build the release binary**

Run:
```bash
CGO_ENABLED=0 go build -o bin/mtga-collection-export ./cmd/mtga-collection-export
./bin/mtga-collection-export --version
```
Expected: prints the version.

- [ ] **Step 3: Manual end-to-end (requires Arena running — optional, document result)**

With Arena open, Collection tab scrolled:
```bash
./bin/mtga-collection-export --out /tmp/mtga_collection.csv --verbose
```
Expected: prints `VALIDATED (...)`, writes a CSV, reports printings/names/copies. If it prints the refusal message, note whether opening/scrolling the Collection tab changes the result (handoff open question §8.2). **Do not fabricate this result** — if Arena is not available in this environment, record the step as skipped.

- [ ] **Step 4: Commit any fixes**

```bash
git add -A
git commit --no-gpg-sign -m "chore: build, vet, and test the full pipeline"
```

---

## Task 12: README, LICENSE, disclaimer

**Files:**
- Create: `README.md`
- Create: `LICENSE` (MIT, current year, `Alexey Vilenski`)

**Interfaces:** none.

README sections (modeled on PBernaerts/mtga-collection-export, handoff §11): Overview, Requirements, Install, Usage, How it works, Troubleshooting, Development, Disclaimer.

- [ ] **Step 1: Write `LICENSE`**

Standard MIT text, `Copyright (c) 2026 Alexey Vilenski`.

- [ ] **Step 2: Write `README.md`**

Must include, verbatim in spirit, this disclaimer (handoff §11, modeled on PBernaerts/mtga-collection-export):

```markdown
## Disclaimer

This is an unofficial fan project. It is **not affiliated with, endorsed, sponsored,
or approved by Wizards of the Coast**. Magic: The Gathering, MTG Arena, and all
related names are trademarks of Wizards of the Coast LLC.

`mtga-collection-export` reads MTG Arena's process memory **read-only**, on your own
machine, of your own data. It does not modify any game file, does not touch network
traffic, does not affect gameplay, and transmits nothing anywhere — everything runs
locally and offline.

Wizards of the Coast does not sanction reading the game's memory (they tolerate
log-based trackers), and in 2021 they deliberately removed the collection endpoint
that older tools relied on. This tool exists because that data is no longer exposed
any other way on macOS. Use it at your own risk. This project bundles nothing that
automates gameplay.

Released under the MIT License. Portions of the approach are documented in
`docs/superpowers/specs/`.
```

Overview: what it exports (CSV of grpId + owned count), the two honest figures (printings vs distinct names). Requirements: macOS, Arena installed & running, Xcode Command Line Tools (`xcode-select --install`) for `lldb`. Install: `go build` / `go install`. Usage: the flags from Task 10, and the note to open + scroll the Collection tab first (handoff §8.2). How it works: plain-language signature-scan + oracle (handoff §5), and why log parsing / `StartHook.Decks` / UnitySpy don't work on macOS (handoff §3, one sentence each). Troubleshooting: "MTGA is not running", "no StartHook line" (enable Detailed Logs), "no validated collection table" (open + scroll Collection, then retry). Development: `CGO_ENABLED=0 go test ./...`, the synthetic-core test as the primary gate.

- [ ] **Step 3: Verify links and build once more**

Run: `CGO_ENABLED=0 go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add README.md LICENSE
git commit --no-gpg-sign -m "docs: README, MIT license, and WotC non-affiliation disclaimer"
```

---

## Self-Review

**Spec coverage:**
- L1 acquire → Task 9. L2 machocore/scan/oracle → Tasks 2, 4, 6. L3 carddb/export → Tasks 7, 8. StartHook bootstrap → Task 5. CLI → Task 10. Synthetic-core gate → Task 3 + Task 10 end-to-end. Docs/disclaimer/MIT → Task 12. Build/vet/manual → Task 11. ✅ every spec §3–§7 layer and §9 acceptance test maps to a task.
- Acceptance tests (spec §6): synthetic-core exact recovery → Task 4 test + Task 10 end-to-end; oracle presence → Task 6; negative/absent → Task 6 + Task 10 `--absent`; structural → Task 6; determinism → Task 3 `TestBuildIsDeterministic` + same-core-same-CSV implied by pure `run`. ✅
- Element-wise-max reconcile (handoff §7.8) → Task 6 `TestEvaluateSelectsAndReconcilesMax`. ✅
- Rebalanced filter (handoff §6) → Task 7 + applied in Task 10. ✅
- Three-figure report (handoff §6) → Task 8 + printed in Task 10. ✅

**Placeholder scan:** No TBD/TODO. The one genuine unknown — StartHook deck card-list field names (Task 5) — is called out explicitly with a verification step and a safety net, not hidden. ✅

**Type consistency:** `machocore.Core`/`Segment`/`ReadU32`/`SegmentBytes` used consistently in Tasks 2/4. `scan.Candidate{Base,Stride,CountOffset,Entries}` identical in Tasks 4/6. `oracle.Oracle`/`Result` fields match Tasks 6/10. `carddb.Card` fields match Tasks 7/8/10. `export.Row`/`Report`/`Write` match Tasks 8/10. `Options` struct identical in Tasks 10 run.go/main.go. ✅

**Known risk carried forward:** Task 5's StartHook schema and Task 10's degraded-mode test both have explicit fallbacks written into the steps. The `run` fallback in Task 10 (`validate`) calls `Evaluate` a second time to surface evidence — implementers should keep the first error rather than re-scanning if performance matters; noted but not blocking.
```
