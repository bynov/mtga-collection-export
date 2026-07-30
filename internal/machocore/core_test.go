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
	binary.LittleEndian.PutUint64(seg[24:], vmBase)            // vmaddr
	binary.LittleEndian.PutUint64(seg[32:], uint64(len(data))) // vmsize
	binary.LittleEndian.PutUint64(seg[40:], fileoff)           // fileoff
	binary.LittleEndian.PutUint64(seg[48:], uint64(len(data))) // filesize
	binary.LittleEndian.PutUint32(seg[56:], 0x7)               // maxprot rwx
	binary.LittleEndian.PutUint32(seg[60:], 0x3)               // initprot rw-

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
