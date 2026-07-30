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

// Core is a core dump opened for read-only word access. Not safe for concurrent use.
type Core struct {
	f     *os.File
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
	if magic := binary.LittleEndian.Uint32(hdr[0:]); magic != mhMagic64 {
		f.Close()
		return nil, fmt.Errorf("not a 64-bit Mach-O core: magic=%#x", magic)
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
