package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// EROFSDD1 (still loaded) has no directory table and only root-level files.
const idxMagic = "EROFSDD2"

// rawFlag marks a chunk stored uncompressed (plen == llen). plen 0 is a hole.
const rawFlag = 1 << 31

// rec is one unique chunk. pstart is the absolute byte offset in the image.
type rec struct {
	hash   [32]byte
	pstart uint64
	plen   uint32 // compressed length | rawFlag; 0 = hole
	llen   uint32
}

type file struct {
	name   string // full path inside the image, no leading slash
	size   uint64
	mtime  int64
	nsec   uint32
	chunks []uint32 // rec ids in logical order
}

// directory is a directory other than the root (which has no stored mtime).
type directory struct {
	path  string // no leading slash
	mtime int64
	nsec  uint32
}

// index is the sidecar: everything needed to rewrite the metadata and dedup.
type index struct {
	uuid    [16]byte
	label   [16]byte
	dataEnd uint64 // end of the last pcluster; metadata starts at round_up(dataEnd)
	blocks  uint64 // image size in blocks at the last commit
	dict    uint32 // largest LZMA dict used so far (goes into the sb cfg)
	recs    []rec
	files   []file
	dirs    []directory         // sorted by path, so parents come first
	m       map[[32]byte]uint32 // hash -> rec id, built on load
}

func loadIndex(path string) (*index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var hdr [8 + 16 + 16 + 8 + 8 + 4 + 8 + 8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	le := binary.LittleEndian
	var ndirs uint64
	switch string(hdr[:8]) {
	case "EROFSDD1":
	case idxMagic:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ndirs = le.Uint64(b[:])
	default:
		return nil, fmt.Errorf("%s: not an erofs-dedup index", path)
	}
	x := &index{dataEnd: le.Uint64(hdr[40:]), blocks: le.Uint64(hdr[48:]), dict: le.Uint32(hdr[56:])}
	copy(x.uuid[:], hdr[8:])
	copy(x.label[:], hdr[24:])
	nrec, nfiles := le.Uint64(hdr[60:]), le.Uint64(hdr[68:])
	isDir := map[string]bool{"": true}
	// parentOK: p is a valid path whose parent is root or a directory read so far.
	parentOK := func(p string) bool {
		c, err := cleanName(p)
		return err == nil && c == p && isDir[parentDir(p)]
	}
	for range ndirs {
		var b [2 + 8 + 4]byte
		if _, err := io.ReadFull(r, b[:2]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		name := make([]byte, le.Uint16(b[:]))
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := io.ReadFull(r, b[2:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		d := directory{string(name), int64(le.Uint64(b[2:])), le.Uint32(b[10:])}
		if !parentOK(d.path) || (len(x.dirs) > 0 && x.dirs[len(x.dirs)-1].path >= d.path) {
			return nil, fmt.Errorf("%s: corrupt directory table", path)
		}
		isDir[d.path] = true
		x.dirs = append(x.dirs, d)
	}
	x.recs = make([]rec, nrec)
	x.m = make(map[[32]byte]uint32, nrec)
	var b [48]byte
	for i := range x.recs {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		c := &x.recs[i]
		copy(c.hash[:], b[:32])
		c.pstart, c.plen, c.llen = le.Uint64(b[32:]), le.Uint32(b[40:]), le.Uint32(b[44:])
		x.m[c.hash] = uint32(i)
	}
	for range nfiles {
		var fh [2]byte
		if _, err := io.ReadFull(r, fh[:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		name := make([]byte, le.Uint16(fh[:]))
		var fb [8 + 8 + 4 + 8]byte
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := io.ReadFull(r, fb[:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if !parentOK(string(name)) || isDir[string(name)] {
			return nil, fmt.Errorf("%s: bad file name %q", path, name)
		}
		fl := file{name: string(name), size: le.Uint64(fb[:]), mtime: int64(le.Uint64(fb[8:])), nsec: le.Uint32(fb[16:])}
		fl.chunks = make([]uint32, le.Uint64(fb[20:]))
		if err := binary.Read(r, le, fl.chunks); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, id := range fl.chunks {
			if id >= uint32(nrec) {
				return nil, fmt.Errorf("%s: corrupt chunk id", path)
			}
		}
		x.files = append(x.files, fl)
	}
	return x, nil
}

// save writes the index to a temp file, fsyncs it and renames it over path.
func (x *index) save(path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	w := bufio.NewWriterSize(tmp, 1<<20)
	le := binary.LittleEndian
	var hdr [76]byte
	copy(hdr[:], idxMagic)
	copy(hdr[8:], x.uuid[:])
	copy(hdr[24:], x.label[:])
	le.PutUint64(hdr[40:], x.dataEnd)
	le.PutUint64(hdr[48:], x.blocks)
	le.PutUint32(hdr[56:], x.dict)
	le.PutUint64(hdr[60:], uint64(len(x.recs)))
	le.PutUint64(hdr[68:], uint64(len(x.files)))
	w.Write(hdr[:])
	var nd [8]byte
	le.PutUint64(nd[:], uint64(len(x.dirs)))
	w.Write(nd[:])
	slices.SortFunc(x.dirs, func(a, b directory) int { return strings.Compare(a.path, b.path) })
	for _, d := range x.dirs {
		var db [2 + 8 + 4]byte
		le.PutUint16(db[:], uint16(len(d.path)))
		le.PutUint64(db[2:], uint64(d.mtime))
		le.PutUint32(db[10:], d.nsec)
		w.Write(db[:2])
		w.WriteString(d.path)
		w.Write(db[2:])
	}
	var b [48]byte
	for _, c := range x.recs {
		copy(b[:], c.hash[:])
		le.PutUint64(b[32:], c.pstart)
		le.PutUint32(b[40:], c.plen)
		le.PutUint32(b[44:], c.llen)
		w.Write(b[:])
	}
	for _, f := range x.files {
		var fb [2 + 8 + 8 + 4 + 8]byte
		le.PutUint16(fb[:], uint16(len(f.name)))
		le.PutUint64(fb[2:], f.size)
		le.PutUint64(fb[10:], uint64(f.mtime))
		le.PutUint32(fb[18:], f.nsec)
		le.PutUint64(fb[22:], uint64(len(f.chunks)))
		w.Write(fb[:2])
		w.WriteString(f.name)
		w.Write(fb[2:])
		binary.Write(w, le, f.chunks)
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
