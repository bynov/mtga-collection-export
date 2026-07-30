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
	if opts.KeepCore {
		fmt.Fprintln(os.Stderr, "working directory:", tmp)
	} else {
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
