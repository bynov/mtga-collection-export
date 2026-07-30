# Design: Windows support for `mtga-collection-export`

**Date:** 2026-07-30
**Status:** Approved design, ready for implementation planning.
**Prerequisite reading:** `docs/superpowers/specs/2026-07-30-mtga-collection-export-design.md` (the macOS design; §8 already reserves room for this: "L1 is the only blocker").

---

## 1. Goal

Make the existing read-only, offline collection exporter work on **Windows** (10/11,
x64), producing the same validated CSV it produces on macOS — no account login, no
network, no modification of the game, no admin/sudo.

**Non-goals:** Linux support (falls out almost for free later but is not in scope
here); any change to the macOS behavior beyond a mechanical refactor; any write-side
operation.

**Success =** on a Windows machine with Arena running, the tool produces a
**validated** CSV whose counts match what Arena actually owns, and **refuses to emit
anything** when validation fails — identical guarantees to macOS.

---

## 2. Core realization

Only **L1 (`acquire`)** is OS-bound. The middle and bottom layers
(`scan` / `oracle` / `carddb` / `export`) scan **raw memory bytes** for a packed
`{grpId, count}` table at some stride, validate against an oracle, and resolve against
SQLite. None of that knows or cares whether the memory image came from an IL2CPP
(macOS) or Mono (Windows) build.

Crucially, Mono's `Dictionary<int,int>` backs its entries with a **fixed-stride struct
array** (`struct Entry { int hashCode; int next; TKey key; TValue value; }`) — exactly
the shape the scanner hunts (grpId at one offset, count at a fixed delta, constant
stride). The stride set `{8,12,16,20,24,28,32,40,48,64}` and count offsets
`{-16..+16}` already cover typical managed layouts. So the existing scanner has a real
chance of working **unchanged** on a Windows memory image.

The README's "Windows trackers read Mono internals, that can't work on macOS" describes
*other tools'* approach (UnitySpy metadata walking). This tool's signature-scan approach
is layout-agnostic and can run on either OS's memory image.

---

## 3. Architecture

Three changes: a memory-source abstraction, a Windows minidump parser, and a Windows
acquisition path. The portable layers are untouched except for the interface swap.

```
cmd/mtga-collection-export/       depends on memimage.Source (was *machocore.Core)
internal/
  memimage/          NEW  shared: Segment struct + Source interface + Open() dispatcher
      memimage.go         Segment, Source; Open(path) sniffs magic → machocore | minidump
  machocore/         macOS core parser — now produces memimage.Segment, satisfies Source
  minidump/          NEW  Windows .dmp parser (portable) — produces Segment, satisfies Source
  acquire/           L1, split by build tag
      acquire.go          untagged shared: StartHook extraction, balancedObject, CopyCardDB
      acquire_darwin.go   lldb, pgrep, ~/Library paths           (moved from acquire.go)
      acquire_windows.go  MiniDumpWriteDump, MTGA.exe discovery, %LocalLow% + install paths
  scan/ oracle/ carddb/ export/ starthook/   unchanged (scan now takes memimage.Source)
internal/testminidump/  NEW  synthetic-minidump builder for the primary Windows test gate
```

### 3.1 `internal/memimage` (the seam)

```go
type Segment struct { VMAddr, FileOff, Size uint64; InitProt uint32 }

type Source interface {
    Segments() []Segment
    SegmentBytes(Segment) ([]byte, error)
    ReadU32(addr uint64) (uint32, bool)
    Close() error
}

// Open sniffs the file's magic and returns the right parser:
//   0xFEEDFACF -> machocore (Mach-O core)
//   "MDMP"     -> minidump  (Windows minidump)
func Open(path string) (Source, error)
```

- `Open` dispatching on magic bytes means `--core FILE` accepts **either** format on
  **either** OS. A macOS dev can inspect a Windows user's `.dmp`, and vice-versa —
  valuable for bug reports.
- Both parsers are pure-Go file readers with no OS syscalls, so both **compile and run
  on every platform**. Only the *acquisition* of a fresh dump is OS-specific.
- `machocore.Core` moves its `Segment` type to `memimage` (or aliases it) and keeps its
  methods; the method set already matches `Source`.

### 3.2 `internal/minidump` (portable, ~80 LOC)

Parse only what the scanner needs:

1. `MINIDUMP_HEADER` — verify signature `MDMP` (`0x504D444D` LE) and read the stream
   directory offset/count.
2. Walk the stream directory. Locate:
   - **`Memory64ListStream` (type 9):** `{ NumberOfMemoryRanges u64; BaseRva u64; ranges [{StartOfMemoryRange u64; DataSize u64}] }`. Data for range *i* starts at
     `BaseRva + sum(DataSize[0..i-1])`. Map each range → `memimage.Segment{VMAddr:StartOfMemoryRange, FileOff:<computed>, Size:DataSize}`.
   - **`MemoryInfoListStream` (type 16):** per-region `State`/`Protect`. Filter segments
     to `MEM_COMMIT` with a writable protection (`PAGE_READWRITE`, `PAGE_WRITECOPY`,
     `PAGE_EXECUTE_READWRITE`, `PAGE_EXECUTE_WRITECOPY`) — parity with machocore's
     `initprot & VM_PROT_WRITE`, and it keeps candidate count / scan time down.
     If this stream is absent, fall back to scanning all captured ranges (safe: the
     scan is signature- and oracle-gated).
3. Implement `Segments`/`SegmentBytes`/`ReadU32` over the file exactly like
   `machocore` (sort by VMAddr, binary-search `ReadU32`).

Hand-rolled, no new dependency — matches the project's minimal-deps ethos (pure-Go
sqlite, hand-rolled Mach-O parser).

### 3.3 `acquire` split by build tag

`acquire.go` keeps the untagged portable helpers already present: `ExtractStartHook`,
`balancedObject`, `CopyCardDB`. The OS-specific functions move behind build tags so the
same call sites work on both platforms.

**`acquire_darwin.go`** (`//go:build darwin`) — verbatim move of today's code:
`FindPID` (pgrep `MacOS/MTGA`), `DumpCore` (lldb `process save-core --style full`),
`FindCardDB` (`~/Library/Application Support/com.wizards.mtga/Downloads/Raw`),
`PlayerLogPath` (`~/Library/Logs/Wizards Of The Coast/MTGA/Player.log`).

**`acquire_windows.go`** (`//go:build windows`), pure Go + `golang.org/x/sys/windows`:

- **`FindPID` + exe path:** enumerate processes via `CreateToolhelp32Snapshot` /
  `Process32First`/`Next` for `MTGA.exe`; obtain the full image path via
  `QueryFullProcessImageName`. Return both (path feeds card-DB discovery).
- **`DumpCore`:** `OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, …, pid)` →
  `MiniDumpWriteDump(hProc, pid, hFile, MiniDumpWithFullMemory, nil, nil, nil)` from
  `dbghelp.dll` (ships with Windows). The API snapshots consistently (suspends the
  target only during capture, then it resumes) — the same clean model as lldb
  `--style full`. **No admin:** dumping a same-user process needs no `SeDebugPrivilege`.
  Verify the target is still alive and the `.dmp` is non-empty afterward.
- **`FindCardDB`:** derive from the running exe's directory →
  `<exeDir>\MTGA_Data\Downloads\Raw\Raw_CardDatabase_*.mtga`, newest by mtime. Robust
  across install locations (Program Files, Epic, custom) because it comes from the live
  process. `--carddb` override remains the fallback.
- **`PlayerLogPath`:** `%USERPROFILE%\AppData\LocalLow\Wizards Of The Coast\MTGA\Player.log`
  (Unity `persistentDataPath` convention — fixed and reliable).

`cmd/.../run.go` replaces `machocore.Open(o.Core)` with `memimage.Open(o.Core)`; the
acquisition orchestration in `main.go` calls the same `acquire.*` names on both OSes.

---

## 4. Testing

- **`internal/testminidump` (primary Windows gate):** mirror of `internal/testcore`.
  Build a synthetic minidump (header + stream directory + `Memory64ListStream` +
  `MemoryInfoListStream`) with a planted `{grpId,count}` table plus the four decoys
  (full card DB, precon decklist, craftability `int[]`, known-absent table). Assert the
  scan+oracle pipeline recovers the planted collection exactly and rejects every decoy.
  **Portable — runs on any CI**, no running game required.
- **`minidump` unit tests:** stream-directory walk, `Memory64List` RVA math, the
  writable-region filter, `ReadU32` boundary behavior.
- **`memimage` dispatch test:** magic-byte sniffing routes to the correct parser.
- **Existing macOS `testcore` gate stays**; the whole suite is portable Go and runs on
  both OSes (only the live lldb/MiniDumpWriteDump acquisition is untestable without a
  game, and it has no unit test today either).
- **Final acceptance (manual, on Windows 10):** run against live Arena, confirm counts
  match the real collection — the Windows analogue of the macOS 815-printings/672-names
  verification.

---

## 5. CI / release / docs

- **`release.yml`:** add `windows/amd64` (`GOOS=windows GOARCH=amd64 CGO_ENABLED=0
  go build -o mtga-collection-export.exe`), include its SHA in `SHA256SUMS`.
- **`ci.yml`:** add `GOOS=windows go build ./...` and `GOOS=windows go vet ./...` so the
  Windows path type-checks on every push; run the portable test suite (and, if a Windows
  runner is available, run it there too).
- **`README.md`:** add a Windows section — requirements (just the binary; Arena running;
  no admin; no external tools — `MiniDumpWriteDump` ships with Windows), install, usage,
  and a troubleshooting note that AV/EDR may heuristically flag process-memory reads
  (benign here; read-only, same-user, no injection). Note the shared "how it works" now
  covers both OSes; only the acquisition step differs.

---

## 6. Open risk (stated plainly)

Whether real Windows Arena keeps its in-memory collection as a **scan-findable stride
table** is unverified until the tool runs against a live Windows client. The Mono
`Dictionary<int,int>` struct-array backing makes this likely, but boxed values or an
alternate container could defeat the current stride/offset search. Mitigations already
in the design: the scan is assumption-free (re-derives stride + count-offset per run),
the oracle refuses anything it can't validate, and `--keep-core` lets a failing run
preserve the `.dmp` for offline analysis. **This is why implementation and verification
happen on the Windows machine** — so the loop (run → inspect → adjust scan params →
re-run) is tight rather than a build/transfer round-trip.

If the default stride/offset set misses the real table, the likely fix is small:
extend `scan.strides` / `scan.countOffsets`, or widen the count sanity bound — all
localized to the already-portable `scan` package, no architectural change.

---

## 7. What we deliberately do NOT do (YAGNI)

- **No UnitySpy / Mono metadata walker** — the signature scan is strictly less brittle
  and already written; reuse it.
- **No external dump tool** (procdump/Sysinternals) — `MiniDumpWriteDump` is built in.
- **No admin-elevation flow** — same-user dump needs none; if a user's environment
  somehow requires `SeDebugPrivilege`, document it rather than auto-elevating.
- **No live `ReadProcessMemory` streaming path** — writing a minidump preserves the
  `--core` / `--keep-core` reproducibility story and the frozen-snapshot model at parity
  with macOS. Revisit only if dump size becomes a real problem.
- **No Linux code** in this deliverable (it becomes a small follow-on: Linux Arena is
  also Mono; a `/proc/<pid>/maps` + `mem` reader would be the L1 addition).
```
