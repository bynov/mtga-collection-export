// Package acquire is the macOS-specific layer: it locates the running MTGA
// process and its data files, and snapshots memory via lldb. Read-only.
package acquire

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const startHookMarker = "<== StartHook"

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

// ExtractStartHook returns the JSON object of the last `<== StartHook` entry.
// The real log format is `<== StartHook(<uuid>)` followed by the JSON, which
// may begin on the next line — so we find the first '{' after the marker and
// brace-balance to its matching '}'.
func ExtractStartHook(logPath string) ([]byte, error) {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return nil, err
	}
	log := string(raw)
	marker := strings.LastIndex(log, startHookMarker)
	if marker < 0 {
		return nil, fmt.Errorf("no StartHook line in %s (enable Detailed Logs and restart Arena)", logPath)
	}
	start := strings.IndexByte(log[marker:], '{')
	if start < 0 {
		return nil, fmt.Errorf("StartHook marker had no JSON body in %s", logPath)
	}
	start += marker
	obj, ok := balancedObject(log[start:])
	if !ok {
		return nil, fmt.Errorf("StartHook JSON in %s was truncated", logPath)
	}
	return []byte(obj), nil
}

// balancedObject returns the JSON object starting at s[0]=='{', respecting
// strings and escapes, or ok=false if braces never balance.
func balancedObject(s string) (string, bool) {
	depth := 0
	inStr := false
	esc := false
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
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
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
