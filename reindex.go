package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// reindex rebuilds a lost sidecar index from the image alone. It parses only
// the exact layout writeMeta produces, not EROFS in general.
func reindex(imgPath, idxPath string, jobs int) error {
	if jobs < 1 {
		return errors.New("-j must be >= 1")
	}
	if _, err := os.Lstat(idxPath); err == nil {
		return fmt.Errorf("%s already exists", idxPath)
	}
	img, err := os.Open(imgPath)
	if err != nil {
		return err
	}
	defer img.Close()
	if err := syscall.Flock(int(img.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is in use: %w", imgPath, err)
	}
	read := func(off uint64, n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := img.ReadAt(b, int64(off)); err != nil {
			return nil, fmt.Errorf("%s: read %d bytes at %d: %w", imgPath, n, off, err)
		}
		return b, nil
	}

	b0, err := read(0, blkSize)
	if err != nil {
		return err
	}
	sb := b0[1024:]
	if le.Uint32(sb) != 0xE0F5E1E2 {
		return errors.New("no EROFS superblock")
	}
	if le.Uint32(sb[80:]) != 0x83 || le.Uint16(sb[84:]) != 1<<1 || sb[12] != 12 || le.Uint16(sb[128:]) != 14 {
		return errors.New("not an image written by erofs-dedup")
	}
	if sbChecksum(sb) != le.Uint32(sb[4:]) {
		return errors.New("superblock checksum mismatch")
	}
	x := &index{blocks: uint64(le.Uint32(sb[36:])), dict: le.Uint32(sb[130:])}
	x.dataEnd = x.blocks * blkSize // the committed end, as add saves it
	copy(x.uuid[:], sb[48:])
	copy(x.label[:], sb[64:])
	meta := uint64(le.Uint32(sb[40:])) * blkSize
	inode := func(nid uint64) ([]byte, error) {
		b, err := read(meta+nid*32, 64)
		if err == nil && b[0]&1 == 0 {
			err = fmt.Errorf("nid %d: not an extended inode", nid)
		}
		return b, err
	}

	// Directories: FLAT_PLAIN blocks, root first, contiguous up to the
	// metadata area. Walk the tree from the root, collecting full paths.
	var ents []dirent // files
	var rootBlk, nDirBlk uint64
	seen := map[uint64]bool{}
	var walk func(nid uint64, p string) error
	walk = func(nid uint64, p string) error {
		if seen[nid] {
			return fmt.Errorf("directory nid %d linked twice", nid)
		}
		seen[nid] = true
		ino, err := inode(nid)
		if err != nil {
			return err
		}
		dirSize := le.Uint64(ino[8:])
		dirBlk := uint64(le.Uint32(ino[16:])) | uint64(le.Uint16(ino[6:]))<<32
		if p == "" {
			rootBlk = dirBlk
		} else {
			x.dirs = append(x.dirs, directory{p, int64(le.Uint64(ino[32:])), le.Uint32(ino[40:])})
		}
		if le.Uint16(ino)>>1&7 != 0 || le.Uint16(ino[4:])&0o170000 != 0o40000 || dirSize == 0 ||
			dirBlk < rootBlk || dirBlk*blkSize+roundUp(dirSize, blkSize) > meta {
			return fmt.Errorf("/%s: unexpected directory layout", p)
		}
		nDirBlk += roundUp(dirSize, blkSize) / blkSize
		// ponytail: one directory in RAM at a time (~20 B per entry), fine for any sane file count.
		dir, err := read(dirBlk*blkSize, int(roundUp(dirSize, blkSize)))
		if err != nil {
			return err
		}
		for bs := uint64(0); bs < dirSize; bs += blkSize {
			blk := dir[bs : bs+blkSize]
			// Names hold no NULs and the block is zero-padded after the last one.
			used := int(min(dirSize-bs, blkSize))
			if bs+blkSize < dirSize {
				used = len(bytes.TrimRight(blk, "\x00"))
			}
			n := int(le.Uint16(blk[8:])) / 12
			if n == 0 || 12*n > used {
				return fmt.Errorf("/%s: corrupt directory block", p)
			}
			for i := range n {
				d := blk[12*i:]
				start, end := int(le.Uint16(d[8:])), used
				if i+1 < n {
					end = int(le.Uint16(d[20:]))
				}
				if start >= end || end > used || bytes.IndexByte(blk[start:end], '/') >= 0 {
					return fmt.Errorf("/%s: corrupt directory entry", p)
				}
				name := string(blk[start:end])
				if name == "." || name == ".." {
					continue
				}
				full := path.Join(p, name)
				switch d[10] {
				case 1:
					ents = append(ents, dirent{full, le.Uint64(d), 1})
				case 2:
					if err := walk(le.Uint64(d), full); err != nil {
						return err
					}
				default:
					return fmt.Errorf("/%s is not a regular file or directory", full)
				}
			}
		}
		return nil
	}
	if err := walk(uint64(le.Uint16(sb[14:])), ""); err != nil {
		return err
	}
	if (rootBlk+nDirBlk)*blkSize != meta {
		return errors.New("directory blocks don't end where the metadata area starts")
	}
	slices.SortFunc(x.dirs, func(a, b directory) int { return strings.Compare(a.path, b.path) })
	// Inodes were written in file order, so nid order is the original order.
	slices.SortFunc(ents, func(a, b dirent) int { return cmp.Compare(a.nid, b.nid) })

	// Pass 1: metadata only. Holes are keyed by llen (pstart 0), data chunks
	// by pstart; rec ids follow extent order so pass 2 reads the image in order.
	ids := map[rec]uint32{} // rec without hash -> id
	var total, end uint64   // stored bytes of the nData unique data chunks, end of the last one
	nData := 0
	for _, e := range ents {
		ino, err := inode(e.nid)
		if err != nil {
			return err
		}
		f := file{name: e.name, size: le.Uint64(ino[8:]), mtime: int64(le.Uint64(ino[32:])), nsec: le.Uint32(ino[40:])}
		switch layout := le.Uint16(ino) >> 1 & 7; {
		case layout == 0 && f.size == 0:
		case layout == 1:
			h, err := read(meta+e.nid*32+64, 32)
			if err != nil {
				return err
			}
			n := uint64(le.Uint32(h)) | uint64(le.Uint16(h[6:]))<<32
			if le.Uint16(h[4:]) != 0x0007 || n == 0 || n > f.size/4096+1 { // chunks are >= 4096 but the last
				return fmt.Errorf("%s: unexpected extent header", f.name)
			}
			// ponytail: one file's extent list in RAM (32 B per chunk, 128 MiB for a
			// 2 TiB file at 512K chunks); stream it in blocks if that ever hurts.
			ext, err := read(meta+e.nid*32+96, int(32*n))
			if err != nil {
				return err
			}
			for j := range n {
				r := ext[32*j:]
				plen, pstart, lstart := le.Uint32(r), le.Uint64(r[4:]), le.Uint64(r[12:])
				next := f.size
				if j+1 < n {
					next = le.Uint64(r[32+12:])
				}
				// ponytail: llen capped at this tool's -chunk-max limit, not EROFS's.
				if (j == 0 && lstart != 0) || next <= lstart || next-lstart > 983040 {
					return fmt.Errorf("%s: bad extent %d", f.name, j)
				}
				c := rec{pstart: pstart, llen: uint32(next - lstart)}
				switch {
				case plen == 0: // hole
					c.pstart = 0
				case plen>>28 == 0 && plen == c.llen:
					c.plen = plen | rawFlag
				case plen>>28 == fmtLZMA>>28 && plen&0x1FFFFF != 0:
					c.plen = plen & 0x1FFFFF
				default:
					return fmt.Errorf("%s: unsupported extent %d (plen %#x)", f.name, j, plen)
				}
				id, ok := ids[c]
				if !ok {
					id = uint32(len(x.recs))
					ids[c] = id
					if c.plen == 0 {
						c.hash = sha256.Sum256(zeros[:c.llen])
					} else {
						end = max(end, c.pstart+uint64(c.plen&^rawFlag))
						total += uint64(c.plen &^ rawFlag)
						nData++
					}
					x.recs = append(x.recs, c)
				}
				f.chunks = append(f.chunks, id)
			}
		default:
			return fmt.Errorf("%s: unexpected inode layout %d", f.name, layout)
		}
		x.files = append(x.files, f)
	}
	// Earlier generations' metadata may lie between the data and the directories.
	if end > rootBlk*blkSize {
		return fmt.Errorf("data ends at %d, past the directories at block %d", end, rootBlk)
	}

	// Pass 2: one sequential reader, jobs workers decompress + hash.
	var (
		mu            sync.Mutex
		firstErr      error
		failed        atomic.Bool
		nDone, nBytes atomic.Int64
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		failed.Store(true)
	}
	type rjob struct {
		id   int
		data []byte
	}
	ch := make(chan rjob, 2*jobs)
	go func() {
		defer close(ch)
		for id, c := range x.recs {
			if c.plen == 0 {
				continue
			}
			if failed.Load() {
				return
			}
			b, err := read(c.pstart, int(c.plen&^rawFlag))
			if err != nil {
				fail(err)
				return
			}
			nBytes.Add(int64(len(b)))
			ch <- rjob{id, b}
		}
	}()
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if failed.Load() {
					continue // drain
				}
				c := &x.recs[j.id] // each worker writes only its own recs' hash
				data := j.data
				if c.plen&rawFlag == 0 {
					var err error
					if data, err = decompress(j.data, int(c.llen), int(x.dict)); err != nil {
						fail(fmt.Errorf("chunk at %d: %w", c.pstart, err))
						continue
					}
				}
				c.hash = sha256.Sum256(data)
				nDone.Add(1)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	status := func() {
		fmt.Fprintf(os.Stderr, "reindex: %d/%d chunks, %.0f/%.0f MiB read\n",
			nDone.Load(), nData, float64(nBytes.Load())/(1<<20), float64(total)/(1<<20))
	}
	tick := time.NewTicker(2 * time.Second)
loop:
	for {
		select {
		case <-done:
			break loop
		case <-tick.C:
			status()
		}
	}
	tick.Stop()
	if firstErr != nil {
		return firstErr
	}
	status()

	x.m = make(map[[32]byte]uint32, len(x.recs))
	for i, c := range x.recs {
		x.m[c.hash] = uint32(i)
	}
	return x.save(idxPath)
}
