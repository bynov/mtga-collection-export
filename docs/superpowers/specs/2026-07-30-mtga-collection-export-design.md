# Design: `mtga-collection-export` — MTG Arena collection exporter for macOS

**Date:** 2026-07-30
**Status:** Approved design, ready for implementation planning.
**Source of truth:** `~/Downloads/mtga-handoff/HANDOFF.md` and its `prototype/` reference implementation.

---

## 1. Goal

A read-only, offline, open-source command-line tool that exports a player's full
MTG Arena collection (card identity + owned count) to CSV on macOS — no account
login, no network, no modification of the game.

**Non-goals:** match tracking, draft logging, deck sync, any write-side operation,
Windows/Linux support (the architecture leaves room for it, but this deliverable
is macOS-only).

Success = on a machine with Arena running, the tool produces a **validated** CSV
whose card counts match what Arena actually owns, and **refuses to emit anything**
when validation fails.

---

## 2. Why Go (decision)

The prototype is Python + numpy. We rewrite in Go for one reason that matters to
an open-source end user: **distribution**. A single static binary with no runtime
dependency beyond Apple's own `lldb` (Xcode Command Line Tools) is dramatically
easier to install than "Python 3 + numpy + a virtualenv." The performance-critical
part — the grpId scan — is a vectorized `bool`-LUT pass in numpy; in Go it is a
tight loop over `uint32` slices indexed into a `[]bool`, which is comparable in
speed and needs no dependency.

SQLite access uses **`modernc.org/sqlite`** (pure Go, cgo-free) so the binary
stays statically linkable and trivially cross-compilable. The client DB is opened
read-only on a copy.

Rejected alternative: harden the Python prototype. Faster to a first release, but
leaves every user fighting a Python/numpy install. Not worth it for a tool meant
to be handed to non-developers.

---

## 3. Architecture

Three sharply separated layers. Only L1 is OS-specific. This mirrors §9/§10 of the
handoff, which earned the separation the hard way.

```
cmd/mtga-collection-export/           CLI entrypoint, flag parsing, orchestration, verdict printing
internal/
  acquire/                 L1  macOS-specific  (~150 LOC)
      lldb.go              shell out to `lldb process save-core --style full`
      locate.go            find MTGA pid, Player.log, Raw_CardDatabase, StartHook line
  machocore/               L2  Mach-O core parsing (portable)
      segments.go          LC_SEGMENT_64 parse, writable-segment index (mmap + bisect)
  scan/                    L2  discovery (portable, assumption-free)
      lut.go               valid-grpId bool lookup table
      scan.go              per-segment grpId sighting scan
      cluster.go           cluster by byte-proximity, infer stride, infer count-offset
  oracle/                  L2  validation (portable)
      oracle.go            known-owned / known-absent / structural tests
      bootstrap.go         derive oracle from StartHook played decks
      reconcile.go         element-wise max across passing variants
  carddb/                  L3  metadata (fully portable)
      carddb.go            open copied SQLite, resolve grpId -> card, enum/markup handling
  export/                  L3  output (fully portable)
      csv.go               raw CSV, Moxfield CSV, Manabox CSV; printings/names/copies report
  starthook/               shared  parse StartHook JSON (decks tiering, NonCollectibleCardList)
```

### Data flow

```
locate pid + files ──▶ lldb save-core ──▶ machocore segments ──▶ scan sightings
                                                                        │
   StartHook ──▶ oracle bootstrap ──────────────────────────┐          ▼
                                                             ▼    cluster/stride/offset
                                              validate candidates (oracle + structural)
                                                             │
                                              reconcile (element-wise max)
                                                             │
                                              carddb resolve ──▶ export CSV
                                                             │
                                              FAIL ──▶ print verdict + evidence, emit nothing
```

---

## 4. Component contracts

Each unit has one purpose, a defined interface, and is testable in isolation.

### 4.1 `acquire` (L1, macOS-only)
- **Does:** locate the running MTGA process and its data files; produce a core dump.
- **In:** nothing (discovers everything).
- **Out:** `Snapshot{ CorePath string; CardDBPath string; StartHook []byte }`.
- **Depends on:** `/usr/bin/lldb`, `pgrep`/`ps`, the filesystem paths in §2 of the handoff.
- **Behavior:** `lldb -p <pid> -b -o "process save-core --style full <out>" -o detach -o quit`.
  Verifies Arena is still alive afterward. Copies `Raw_CardDatabase_*.mtga` before
  reading (client holds it open). Extracts the single `<== StartHook` JSON line
  from `Player.log`. Uses `--style full` deliberately (§4/§7.8: modified-memory
  returns stale variants).

### 4.2 `machocore` (L2)
- **Does:** parse a Mach-O core, expose writable segments for random access.
- **Out:** sorted `[]Segment{ VMAddr, FileOff, Size, InitProt }`, filtered to `initprot & 0x2`.
- **Pitfalls pre-solved (§7):** `LC_SEGMENT_64` body is 64 B; fields are `body[16:56]`
  unpacked `<QQQQii`. Segment lookup is `mmap` + `bisect` over vmaddr, never linear
  (a real core has ~4,545 writable segments).

### 4.3 `scan` (L2)
- **Does:** find candidate collection tables without assuming layout.
- **In:** segments + valid-grpId LUT.
- **Out:** `[]Candidate{ Addr, Stride, CountOffset, Entries map[grpId]count }`.
- **Algorithm (§5):** scan 4-byte LE ints against the LUT (align 4, with align-1
  fallback); cluster hits by **byte proximity** (gap ≤ 512 B), not hit contiguity;
  within a cluster group by `addr % stride` for each candidate stride
  `{8,12,16,20,24,28,32,40,48,64}`; for each run try count offsets
  `{-16,-12,-8,-4,+4,+8,+12,+16}`.
- **Performance (§7):** batch slice→`[]bool` lookups; don't build the hit map
  element-by-element.

### 4.4 `oracle` (L2) — the core of correctness
- **Does:** score every candidate; select the winner set; reconcile.
- **Tests (§5 table):**
  1. contains every card proven owned (from played decks)
  2. contains **no** user-asserted absent card
  3. stored count ≥ copies played — **excluding rarity-1 basic lands** (§7.5)
  4. no non-collectible ids (`NonCollectibleCardList`)
  5. no count > 4 for non-basics (Arena deck cap)
  6. count distribution is quantity-shaped, not flag-shaped (kills the `{1,3}` decoy)
- **Bootstrap (`bootstrap.go`):** derive known-owned from `StartHook.Decks` whose
  `Attributes[].LastPlayed` is a real date (§3.2). Classify precon
  (`?=?Loc/Decks/Precon/` prefix) and imported/untrusted decks out. If no oracle
  can be built, run structural tests only and **print a prominent degraded-mode warning**.
- **Reconcile (`reconcile.go`):** across all variants that pass validation, take the
  **element-wise maximum** count per grpId (§7.8 — ownership is monotonic; a lower
  value is a stale/working copy).
- **Gate:** returns either a validated result **or** a failure with an evidence
  table. Never a maybe.

### 4.5 `carddb` (L3)
- **Does:** resolve `grpId -> {name, set, collectorNumber, rarity, colors, types, mana}`.
- **Query (§6):** join `Cards` ↔ `Localizations_enUS` on `TitleId`/`Formatted=1`.
- **Gotchas pre-solved (§6):** rarity int map `1=Basic…5=Mythic`; mana is
  `OldSchoolManaText` `o`-shorthand → `{2}{U}{U}`; comma-separated enum ints via
  `Enums`; strip `<nobr>`/layout markup from names; collectible filter
  `IsPrimaryCard=1 AND IsToken=0` minus `NonCollectibleCardList`.
- **Rebalanced filter:** drop `IsRebalanced=1` rows (§6 — Alchemy rebalanced cards
  mirror-count and double-report). Do **not** dedupe ordinary reprints.

### 4.6 `export` (L3)
- **Does:** write CSV; report totals honestly.
- **Formats:** `raw` (grpId, name, set, collector#, rarity, count) default; plus
  Moxfield- and Manabox-friendly CSV via `--format`.
- **Report (§6):** always print **three** numbers — printings/rows, distinct card
  names, total copies — because calling rows "cards" overstates it. Log every
  filter/cap applied (§9: silent truncation reads as completeness). Surface the
  "basic lands stored as 1 / unlimited" note so users don't think they own one Island.

---

## 5. CLI

```
mtga-collection-export [flags]

  --out FILE         output path (default mtga_collection.csv)
  --format raw|moxfield|manabox   (default raw)
  --absent GRPID,... user-asserted cards they do NOT own (hardens the oracle)
  --keep-core        keep the lldb core dump instead of deleting it
  --core FILE        skip acquisition; scan an existing core (for testing/repro)
  --carddb FILE      override card DB path
  --verbose          print candidate scoring / evidence tables
```

Determinism: same core ⇒ same CSV (§9.5).

---

## 6. Testing (ships with the tool — §9 acceptance tests)

1. **Synthetic-core test (primary gate):** Go port of `make_fake_core.py` builds a
   Mach-O core with a planted `{grpId,count}` table + four decoys (full card DB,
   precon decklist, craftability `int[]`, and a decoy containing known-absent
   cards). Assert exact recovery of all planted cards and rejection of every decoy.
   This catches the scanner bugs without touching the game.
2. **Oracle test:** 100% of played-deck cards present in the winner.
3. **Negative test:** a user-asserted absent card never appears.
4. **Structural tests:** no non-collectible, no >4 non-basic, count ≥ played (non-basic).
5. **Determinism:** same core ⇒ identical CSV bytes.
6. **Unit tests** per package: `machocore` segment parsing (the `[16:56]` slice
   trap), rarity/enum/markup resolution, rebalanced filtering, reconcile max logic.

Unit tests use fixtures (a tiny synthetic SQLite DB, a trimmed StartHook JSON), no
live game required. Only a manual/optional end-to-end test needs a running Arena.

---

## 7. Documentation deliverables

- **README.md** — sections modeled on PBernaerts/mtga-collection-export: Overview, Requirements
  (macOS, Xcode CLT for `lldb`, Arena running), Install, Usage, How it works
  (signature scan + oracle, in plain language), Troubleshooting, Development, Disclaimer.
- **Disclaimer (§11):** unofficial fan project, **not affiliated with or endorsed by
  Wizards of the Coast**; reads Arena process memory read-only; no game files
  modified, no packets touched, no data transmitted; WotC does not sanction memory
  reading (they tolerate log-based trackers) and removed the collection endpoint in
  2021 — stated plainly, user decides. Fan Content Policy acknowledgement.
- **LICENSE** — MIT.

---

## 8. What we deliberately do NOT build (YAGNI / dead ends from §3)

- **No log parsing for ownership** — the endpoint was removed in 2021 (§3.1).
- **No `StartHook.Decks` as ownership** — it is Arena's precon catalog + imported
  lists (§3.2, the trap that shipped bad data). Decks are used *only* to bootstrap
  the oracle.
- **No UnitySpy / metadata walker** — Arena is IL2CPP on macOS; strictly more work
  and more brittle than the scan (§3.3).
- **No hardcoded addresses or offsets** — re-derived every run (§9).
- **No Scryfall / network** — the local SQLite DB is complete and offline (§3.4, §6).
- **No Windows/Linux code** in this deliverable (architecture permits it later; L1
  is the only blocker).

---

## 9. Risks / open items (from §8)

- **Entitlement route untested:** the MVP shells out to Apple-signed `lldb`, which
  is known to work without sudo. A self-signed reader calling `task_for_pid` would
  likely be denied. README documents the `lldb` dependency rather than promising a
  standalone binary that reads memory directly. Signing with
  `com.apple.security.cs.debugger` is a documented future enhancement.
- **Collection-screen scroll:** prototype asked the user to open Collection and
  scroll ~30 s; not ablation-tested. README will instruct it as a precaution and
  note it may be unnecessary.
- **Patch fragility:** mitigated by design — nothing hardcoded, layout re-derived,
  oracle-gated. If Arena changes the struct, the scan adapts or the oracle refuses.
```
