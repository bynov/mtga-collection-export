# mtga-collection-export

Export your **MTG Arena collection** — every card and how many copies you own —
to CSV on **macOS**. No account login, no network, no modification of the game.
Everything runs locally and read-only.

macOS Arena is an IL2CPP build, so the Windows/Linux collection trackers (which
read Mono internals) do not work here, and the game stopped exposing the
collection in its logs back in 2021. This tool takes a different route: it
snapshots the running game's memory with Apple's own `lldb`, finds the collection
table by signature, and **proves the result is correct against facts it already
trusts** before writing anything.

> Reports two honest figures, not one: **printings** (rows, one per art/set) and
> **distinct card names**. A collection of "815 printings" is really ~672 unique
> cards — calling the row count "cards" overstates it.

---

## Requirements

- **macOS** (Apple Silicon or Intel).
- **MTG Arena installed and running**, sitting past the login screen.
- **Xcode Command Line Tools** — provides `lldb`, which does the memory snapshot:
  ```sh
  xcode-select --install
  ```
- **Go 1.26+** to build from source.

No admin/sudo is required: MTGA ships without a hardened runtime, so a same-user
`lldb` can attach to it.

---

## Install

```sh
git clone https://github.com/bynov/mtga-collection-export
cd mtga-collection-export
CGO_ENABLED=0 go build -o bin/mtga-collection-export ./cmd/mtga-collection-export
```

The binary is self-contained (pure-Go SQLite, no cgo). Copy `bin/mtga-collection-export`
anywhere on your `PATH`.

---

## Usage

1. Launch MTG Arena and log in.
2. Open the **Collection** tab and scroll through it once. (This makes sure the
   collection is resident in memory. It may not be strictly required — see
   [Troubleshooting](#troubleshooting) — but it costs nothing.)
3. Run:
   ```sh
   mtga-collection-export --out mtga_collection.csv
   ```

Arena is paused only for the few seconds it takes to write the snapshot, then
resumes cleanly.

```
dumping Arena memory (pauses the game only while the snapshot is written)...
VALIDATED (3 variants reconciled by element-wise max)
wrote mtga_collection.csv: 815 printings, 672 distinct names, 1107 copies
dropped 3 Alchemy-rebalanced duplicate rows
note: basic lands are unlimited and stored as 1 — a count of 1 does not mean you own only one.
```

### Options

| Flag | Default | Meaning |
|---|---|---|
| `--out FILE` | `mtga_collection.csv` | Output path. |
| `--format raw\|moxfield\|manabox` | `raw` | CSV shape. `raw` includes rarity, colors, type, mana cost, and the Arena grpId; `moxfield` and `manabox` match those importers' columns. |
| `--absent GRPID,...` | — | Cards you know you do **not** own. Hardens the validation (see below). |
| `--keep-core` | off | Keep the memory snapshot instead of deleting it. |
| `--core FILE` | — | Scan an existing snapshot instead of dumping a new one (with `--carddb`). For testing / reproducibility. |
| `--carddb FILE` | auto | Override the card database path. |
| `--starthook FILE` | auto | Override the StartHook JSON path. |
| `--verbose` | off | Print how many candidate tables were considered. |
| `--version` | — | Print version and exit. |

### Alchemy-rebalanced cards

Alchemy "rebalanced" cards share ownership with their paper twin (crafting one
increments both), so Arena stores them twice. This tool drops the rebalanced
copies automatically and tells you how many it removed — your counts are not
double-reported.

---

## How it works

Three layers, only the first is macOS-specific:

1. **Acquire.** `lldb process save-core --style full` writes a snapshot of Arena's
   memory. It also copies the client's local card database and reads the one
   `StartHook` line from `Player.log`.
2. **Discover + validate.** The snapshot is scanned for a packed
   `{grpId, count}` table. Nothing is hardcoded — the stride and count offset are
   re-derived every run, so the tool survives game patches and address
   randomization. Every candidate table is then **graded against an oracle** built
   from facts already known:
   - it must contain every card in a deck you have actually played (Arena refuses
     to queue a deck with cards you don't own);
   - it must contain **no** card you've told it you lack (`--absent`), and no
     non-collectible id;
   - counts must be sane (non-basics cap at 4; a played 4-of proves you own 4).

   Memory often holds several stale copies of the collection; the tool takes the
   **element-wise maximum** across every copy that passes validation, because you
   can only ever *gain* cards, so a lower count is an out-of-date snapshot.
3. **Resolve + export.** Surviving grpIds are joined against the local SQLite card
   database (fully offline) and written to CSV, with rebalanced duplicates removed.

**If nothing passes validation, the tool writes nothing** and prints why. It will
never hand you a plausible-but-wrong CSV.

### Why not the simpler routes?

- **Log parsing** can't do it: Arena removed the collection endpoint from its logs
  in 2021, on every platform.
- **`StartHook`'s deck list** is not your collection: 98 of those "decks" are
  Arena's shipped precon catalog, and imported decklists include cards you don't
  own. The tool uses played decks only as a validation signal, never as ownership.
- **The Windows/Linux daemon** (UnitySpy) reads Mono runtime internals. macOS Arena
  is IL2CPP — there is no Mono to read — so that approach simply cannot run here.

---

## Troubleshooting

| Message | Fix |
|---|---|
| `MTGA is not running` | Launch Arena and log in past the title screen, then retry. |
| `no Raw_CardDatabase_*.mtga` | Arena isn't fully installed, or hasn't finished downloading assets. Open Arena once and let it update. |
| `no StartHook line` | Enable **Detailed Logs** (Arena → Settings → Account → *Detailed Logs (Plugin Support)*) and restart Arena. Without it, the tool still runs but falls back to structural checks only and warns you. |
| `no validated collection table found` | Open the **Collection** tab and scroll through your whole collection, then run again. If it persists, the memory layout may have changed in a game patch — please open an issue. |
| `is Xcode Command Line Tools installed?` | Run `xcode-select --install`. |

---

## Development

```sh
CGO_ENABLED=0 go test ./...
```

The primary correctness gate is a **synthetic core** test: it builds a Mach-O
snapshot with a planted collection plus four decoys (the full card DB, a precon
decklist, a craftability list, and a table containing cards you don't own) and
asserts the scanner recovers the collection exactly and rejects every decoy — no
running game required. See `internal/testcore` and `cmd/mtga-collection-export/run_test.go`.

The design and implementation notes live in `docs/superpowers/`.

---

## Disclaimer

This is an unofficial fan project. It is **not affiliated with, endorsed,
sponsored, or approved by Wizards of the Coast**. Magic: The Gathering, MTG Arena,
and all related names are trademarks of Wizards of the Coast LLC. This project is
unofficial Fan Content permitted under the Wizards of the Coast Fan Content
Policy; it is not approved or endorsed by Wizards.

`mtga-collection-export` reads MTG Arena's process memory **read-only**, on your own
machine, of your own data. It does not modify any game file, does not touch
network traffic, does not affect gameplay, and transmits nothing anywhere —
everything runs locally and offline.

Wizards of the Coast does not sanction reading the game's memory (they tolerate
log-based trackers), and in 2021 they deliberately removed the collection endpoint
that older tools relied on. This tool exists because that data is no longer
exposed any other way on macOS. **Use it at your own risk.** This project bundles
nothing that automates gameplay.

Released under the [MIT License](LICENSE).
