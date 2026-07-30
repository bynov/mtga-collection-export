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
	buf := make([]byte, 0, nslots*stride)
	for _, s := range slot {
		rec := make([]byte, stride)
		if s >= 0 {
			binary.LittleEndian.PutUint32(rec[0:], uint32(s)&0x7FFFFFFF) // hash
			binary.LittleEndian.PutUint32(rec[4:], 0xFFFFFFFF)           // next = -1
			binary.LittleEndian.PutUint32(rec[keyOff:], uint32(s))
			binary.LittleEndian.PutUint32(rec[valOff:], pairs[uint32(s)])
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
	for _, g := range sample(rng, s.Valid, minInt(2274, len(s.Valid))) {
		precon[g] = uint32(rng.Intn(4) + 1)
	}
	decoyAbsent := map[uint32]uint32{}
	for _, a := range s.Absent {
		decoyAbsent[a] = 2
	}
	for _, g := range sample(rng, s.Valid, minInt(900, len(s.Valid))) {
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
		{"craftable", flatIDList(sample(rng, s.Valid, minInt(400, len(s.Valid))))},
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
		blob = append(blob, make([]byte, 4096)...)
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
	out := append(append(hdr, segCmds...), body...)
	if _, err := f.Write(out); err != nil {
		return Built{}, err
	}
	return Built{CorePath: path, Truth: truth, Basics: basics}, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
