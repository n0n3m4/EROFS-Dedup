// erofs-dedup builds EROFS images (48-bit layout, MicroLZMA, encoded extents)
// whose files share deduplicated content-defined chunks, e.g. many versions
// of a disk image. See the usage text below.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const usage = `usage:
  erofs-dedup create IMAGE [opts] FILE...   new image + sidecar index
  erofs-dedup add    IMAGE [opts] [FILE...] append files, dedup against the image
                                            (no FILE: just recommit metadata)
  FILE: regular file or block device -> /NAME; directory d -> /d/... (recursive, like cp -r;
  "." -> /); -name PATH puts the single FILE (or a directory's contents) at /PATH
  erofs-dedup ls     IMAGE [--index PATH]   list files and how much they share
  erofs-dedup reindex IMAGE [--index PATH] [-j N]  rebuild a lost sidecar index from the image
  erofs-dedup fixsb   IMAGE [-index PATH]  restore block 0 from the superblock copy at the end
                                            of the image; if that is bad too, rebuild it from the index
`

type opts struct {
	jobs          int
	min, avg, max int
	preset        int
	extreme       bool
	dict          int
	uuid          string
	label         string
	index         string
	name          string
	noSB          bool // fixsb fallback: accept an image without a valid superblock
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, img := os.Args[1], os.Args[2]
	var o opts
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.IntVar(&o.jobs, "j", runtime.NumCPU(), "compression workers")
	fs.IntVar(&o.min, "chunk-min", 256<<10, "minimum chunk size")
	fs.IntVar(&o.avg, "chunk-avg", 512<<10, "average chunk size")
	fs.IntVar(&o.max, "chunk-max", 983040, "maximum chunk size (<= 983040)")
	fs.IntVar(&o.preset, "lzma-preset", 9, "LZMA preset 0..9")
	fs.BoolVar(&o.extreme, "lzma-extreme", false, "LZMA extreme mode")
	fs.IntVar(&o.dict, "lzma-dict", 1<<20, "LZMA dictionary size (4 KiB..8 MiB)")
	fs.StringVar(&o.uuid, "uuid", "", "volume uuid (create only; default random)")
	fs.StringVar(&o.label, "label", "", "volume label, <= 16 bytes (create only)")
	fs.StringVar(&o.index, "index", "", "sidecar index path (default IMAGE.idx)")
	fs.StringVar(&o.name, "name", "", "path inside the image, e.g. 2026-10/sda.img (single FILE only)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	fs.Parse(os.Args[3:])
	explicitIndex := o.index != ""
	if o.index == "" {
		o.index = img + ".idx"
	}
	var err error
	switch cmd {
	case "create", "add":
		if fs.NArg() == 0 && cmd == "create" {
			fs.Usage()
			os.Exit(2)
		}
		err = run(cmd == "create", img, o, fs.Args())
	case "ls":
		err = ls(os.Stdout, o.index)
	case "reindex":
		err = reindex(img, o.index, o.jobs)
	case "fixsb":
		if !explicitIndex {
			o.index = "" // the index fallback is opt-in: a stale index loses data
		}
		err = fixsb(img, o)
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "erofs-dedup:", err)
		os.Exit(1)
	}
}

func run(create bool, imgPath string, o opts, paths []string) error {
	switch {
	case o.jobs < 1:
		return errors.New("-j must be >= 1")
	case o.min < 4096 || o.min >= o.avg || o.avg >= o.max || o.max > 983040:
		return errors.New("need 4096 <= chunk-min < chunk-avg < chunk-max <= 983040")
	case o.preset < 0 || o.preset > 9:
		return errors.New("lzma-preset must be 0..9")
	case o.dict < 4096 || o.dict > 8<<20:
		return errors.New("lzma-dict must be 4 KiB..8 MiB")
	case len(o.label) > 16:
		return errors.New("label longer than 16 bytes")
	case o.name != "" && len(paths) != 1:
		return errors.New("-name needs exactly one FILE")
	}

	var x *index
	var err error
	if create {
		if _, err := os.Stat(o.index); err == nil {
			return fmt.Errorf("%s already exists", o.index)
		}
		x = &index{dataEnd: blkSize, m: map[[32]byte]uint32{}}
		if o.uuid == "" {
			rand.Read(x.uuid[:])
		} else if u, err := hex.DecodeString(strings.ReplaceAll(o.uuid, "-", "")); err != nil || len(u) != 16 {
			return fmt.Errorf("bad uuid %q", o.uuid)
		} else {
			copy(x.uuid[:], u)
		}
		copy(x.label[:], o.label)
	} else if x, err = loadIndex(o.index); err != nil {
		return err
	}
	todos, implicit, err := plan(x, o.name, paths)
	if err != nil {
		return err
	}

	var img *os.File
	if create {
		img, err = os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	} else {
		img, err = os.OpenFile(imgPath, os.O_RDWR, 0)
	}
	if err != nil {
		return err
	}
	defer img.Close()
	if err := syscall.Flock(int(img.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is in use: %w", imgPath, err)
	}
	if !create {
		if err := checkImage(img, x, o.noSB); err != nil {
			return fmt.Errorf("%s: %w", imgPath, err)
		}
		// Drop the old superblock copy and whatever a crashed run appended after
		// the last commit, then append: the committed image stays intact until
		// block 0 is rewritten.
		x.dataEnd = x.blocks * blkSize
		if err := img.Truncate(int64(x.dataEnd)); err != nil {
			return err
		}
	}
	x.dict = max(x.dict, uint32(o.dict))

	for _, t := range todos {
		if err := addFile(img, x, o, t.src, t.name); err != nil {
			return fmt.Errorf("%s: %w", t.src, err)
		}
	}
	now := time.Now()
	for _, p := range implicit {
		x.dirs = append(x.dirs, directory{p, now.Unix(), uint32(now.Nanosecond())})
	}

	// Commit order: data + metadata + superblock copy durable, then sidecar,
	// then block 0. Until the block 0 write the old superblock describes the
	// old image, which nothing here overwrote. A crash before the sidecar
	// rename leaves the old sidecar, and the next add truncates back to its
	// committed end. A crash after it leaves the new sidecar, whose data and
	// metadata are on disk. A torn block 0 is restored from the copy by fixsb.
	blocks, b0, err := writeMeta(img, x)
	if err != nil {
		return err
	}
	// Superblock copy one block past the end: outside sb.blocks, so readers ignore it.
	if _, err := img.WriteAt(b0, int64(blocks*blkSize)); err != nil {
		return err
	}
	if err := img.Sync(); err != nil {
		return err
	}
	x.blocks, x.dataEnd = blocks, blocks*blkSize
	if err := x.save(o.index); err != nil {
		return err
	}
	if _, err := img.WriteAt(b0, 0); err != nil {
		return err
	}
	return img.Sync()
}

type todo struct{ src, name string }

// plan maps the FILE arguments to paths inside the image and adds their
// directories to x.dirs, except the ones -name creates implicitly (returned,
// they get the commit time). It writes nothing, so a collision with anything
// already in the image (or planned) fails before any data is appended.
func plan(x *index, name string, paths []string) (todos []todo, implicit []string, err error) {
	isDir := map[string]bool{"": true} // every path in the image; "" is the root
	for _, d := range x.dirs {
		isDir[d.path] = true
	}
	for _, f := range x.files {
		isDir[f.name] = false
	}
	place := func(p string, dir bool, mt time.Time) error {
		for i := range len(p) {
			if p[i] != '/' {
				continue
			}
			switch d, ok := isDir[p[:i]]; {
			case !ok:
				isDir[p[:i]] = true
				implicit = append(implicit, p[:i])
			case !d:
				return fmt.Errorf("%q is a file in the image", p[:i])
			}
		}
		if d, ok := isDir[p]; ok {
			if d && dir {
				return nil // merge into an existing directory
			}
			return fmt.Errorf("%q already exists in the image", p)
		}
		isDir[p] = dir
		if dir {
			x.dirs = append(x.dirs, directory{p, mt.Unix(), uint32(mt.Nanosecond())})
		}
		return nil
	}
	for _, p := range paths {
		st, err := os.Stat(p) // follows symlinks: /dev/disk/by-id/... must work
		if err != nil {
			return nil, nil, err
		}
		dst := ""
		if name != "" {
			if dst, err = cleanName(name); err != nil {
				return nil, nil, err
			}
		} else if !st.IsDir() || filepath.Clean(p) != "." {
			if dst, err = cleanName(filepath.Base(filepath.Clean(p))); err != nil {
				return nil, nil, fmt.Errorf("%s: %w, use -name", p, err)
			}
		}
		if !st.IsDir() {
			if err := place(dst, false, time.Time{}); err != nil {
				return nil, nil, err
			}
			todos = append(todos, todo{p, dst})
			continue
		}
		root, err := filepath.EvalSymlinks(p) // WalkDir doesn't follow a symlinked root
		if err != nil {
			return nil, nil, err
		}
		err = filepath.WalkDir(root, func(sp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, sp)
			q := dst
			if rel != "." {
				if q, err = cleanName(path.Join(dst, filepath.ToSlash(rel))); err != nil {
					return fmt.Errorf("%s: %w", sp, err)
				}
			}
			switch {
			case d.IsDir():
				info, err := d.Info()
				if err != nil {
					return err
				}
				return place(q, true, info.ModTime())
			case d.Type().IsRegular():
				todos = append(todos, todo{sp, q})
				return place(q, false, time.Time{})
			}
			fmt.Fprintf(os.Stderr, "erofs-dedup: skipping %s: not a regular file or directory\n", sp)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return todos, implicit, nil
}

// cleanName normalises a path inside the image: no leading slash, no empty,
// "." or ".." components, each component <= 255 bytes, no NUL.
func cleanName(s string) (string, error) {
	c := strings.TrimLeft(path.Clean(s), "/")
	bad := c == "" || c == "." || len(c) > 4095 || strings.ContainsRune(s, 0) || slices.Contains(strings.Split(s, "/"), "..")
	for _, part := range strings.Split(c, "/") {
		bad = bad || len(part) > 255
	}
	if bad {
		return "", fmt.Errorf("bad name %q", s)
	}
	return c, nil
}

// checkImage refuses to touch an image that doesn't belong to the sidecar or
// that is newer than it (a stale sidecar would truncate committed data). The
// superblock must be valid, match the uuid and be no newer; the sidecar's last
// stored chunk must be in the image (catches a wrong index with the right uuid).
// noSB skips the superblock checks (fixsb rebuilding a lost superblock).
func checkImage(img *os.File, x *index, noSB bool) error {
	b0 := make([]byte, blkSize)
	if _, err := img.ReadAt(b0, 0); err != nil {
		return err
	}
	sb := b0[1024:]
	switch {
	case noSB:
	case le.Uint32(sb) != 0xE0F5E1E2 || le.Uint32(sb[4:]) != sbChecksum(sb):
		return errors.New("no valid EROFS superblock in block 0 (torn or zeroed? try: erofs-dedup fixsb IMAGE)")
	case !bytes.Equal(sb[48:64], x.uuid[:]):
		return errors.New("superblock uuid doesn't match the sidecar index")
	case uint64(le.Uint32(sb[36:])) > x.blocks:
		return errors.New("image is newer than the sidecar index (stale index?)")
	}
	st, err := img.Stat()
	if err != nil {
		return err
	}
	if uint64(st.Size()) < x.blocks*blkSize {
		return errors.New("image is shorter than the sidecar's committed size")
	}

	var last *rec // the data chunk with the largest pstart
	for i, c := range x.recs {
		if c.plen&^rawFlag != 0 && (last == nil || c.pstart > last.pstart) {
			last = &x.recs[i]
		}
	}
	if last == nil {
		return nil
	}
	n := last.plen &^ rawFlag
	if last.llen > 983040 || n > last.llen || last.pstart+uint64(n) > x.blocks*blkSize {
		return errors.New("corrupt sidecar index")
	}
	b := make([]byte, n)
	if _, err := img.ReadAt(b, int64(last.pstart)); err != nil {
		return err
	}
	if last.plen&rawFlag == 0 {
		b, err = decompress(b, int(last.llen), int(x.dict))
	}
	if err != nil || sha256.Sum256(b) != last.hash {
		return fmt.Errorf("chunk at %d doesn't match the sidecar index (wrong index?)", last.pstart)
	}
	return nil
}

// fixsb restores block 0 from the superblock copy that every commit writes
// as the last block of the image, right after the metadata. If the copy is bad
// too, it recommits the metadata from the sidecar index (like add with no FILE)
// once the index's last chunk checks out against the image.
func fixsb(imgPath string, o opts) error {
	err := restoreSB(imgPath)
	if !errors.Is(err, errNoCopy) {
		return err
	}
	if _, serr := os.Stat(o.index); serr != nil { // also o.index == ""
		return err
	}
	fmt.Fprintf(os.Stderr, "erofs-dedup: %v; rebuilding the superblock and metadata from %s "+
		"(cannot check it is current: a stale index drops newer data)\n", err, o.index)
	o.noSB = true
	return run(false, imgPath, o, nil)
}

var errNoCopy = errors.New("no valid superblock copy in the last block")

func restoreSB(imgPath string) error {
	img, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer img.Close()
	if err := syscall.Flock(int(img.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is in use: %w", imgPath, err)
	}
	st, err := img.Stat()
	if err != nil {
		return err
	}
	off := st.Size()/blkSize*blkSize - blkSize
	if off < blkSize {
		return fmt.Errorf("%s: %w (too small)", imgPath, errNoCopy)
	}
	b0 := make([]byte, blkSize)
	if _, err := img.ReadAt(b0, off); err != nil {
		return err
	}
	sb := b0[1024:]
	if le.Uint32(sb) != 0xE0F5E1E2 || le.Uint32(sb[4:]) != sbChecksum(sb) ||
		int64(le.Uint32(sb[36:]))*blkSize != off {
		return fmt.Errorf("%s: %w", imgPath, errNoCopy)
	}
	if _, err := img.WriteAt(b0, 0); err != nil {
		return err
	}
	return img.Sync()
}

var zeros = make([]byte, 1<<20)

type job struct {
	seq  int
	data []byte
}

type result struct {
	id   uint32
	data []byte
	raw  bool
}

// addFile streams one file through CDC -> hash/dedup/compress -> append.
func addFile(img *os.File, x *index, o opts, path, name string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	size, err := src.Seek(0, io.SeekEnd) // works for block devices too
	if err != nil {
		return err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	mt := st.ModTime()
	fl := file{name: name, mtime: mt.Unix(), nsec: uint32(mt.Nanosecond())}

	var (
		mu                      sync.Mutex // guards x.recs, x.m, fl.chunks, firstErr
		firstErr                error
		failed                  atomic.Bool
		nRead, nHit, nNew, nOut atomic.Int64
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		failed.Store(true)
	}
	var encs []*encoder
	for range o.jobs {
		enc, err := newEncoder(o.preset, o.extreme, o.dict)
		if err != nil {
			for _, e := range encs {
				e.close()
			}
			return err
		}
		encs = append(encs, enc)
	}
	jobs := make(chan job, 2*o.jobs)
	results := make(chan result, 2*o.jobs)

	go func() {
		defer close(jobs)
		err := split(src, o.min, o.avg, o.max, func(c []byte) error {
			if failed.Load() {
				return errors.New("aborted")
			}
			nRead.Add(int64(len(c)))
			mu.Lock()
			fl.chunks = append(fl.chunks, 0)
			seq := len(fl.chunks) - 1
			mu.Unlock()
			jobs <- job{seq, c}
			return nil
		})
		if err != nil {
			fail(err)
		}
	}()

	var wg sync.WaitGroup
	for _, enc := range encs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer enc.close()
			for j := range jobs {
				if failed.Load() {
					continue // drain
				}
				h := sha256.Sum256(j.data)
				mu.Lock()
				id, hit := x.m[h]
				if !hit { // claim; the writer fills pstart/plen
					id = uint32(len(x.recs))
					x.recs = append(x.recs, rec{hash: h, llen: uint32(len(j.data))})
					x.m[h] = id
				}
				fl.chunks[j.seq] = id
				mu.Unlock()
				if hit {
					nHit.Add(int64(len(j.data)))
					continue
				}
				nNew.Add(int64(len(j.data)))
				if bytes.Equal(j.data, zeros[:len(j.data)]) {
					continue // hole: plen 0, nothing stored
				}
				// pclusters are block-aligned: compression must save a block.
				r := result{id, j.data, true}
				if len(j.data) > blkSize {
					dst := make([]byte, roundUp(uint64(len(j.data)), blkSize)-blkSize)
					n, ok, err := enc.compress(dst, j.data)
					if err != nil {
						fail(err)
						continue
					}
					if ok {
						r = result{id, dst[:n], false}
					}
				}
				results <- r
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		for r := range results {
			if failed.Load() {
				continue
			}
			pstart := roundUp(x.dataEnd, blkSize)
			if _, err := img.WriteAt(r.data, int64(pstart)); err != nil {
				fail(err)
				continue
			}
			x.dataEnd = pstart + uint64(len(r.data))
			nOut.Add(int64(len(r.data)))
			plen := uint32(len(r.data))
			if r.raw {
				plen |= rawFlag
			}
			mu.Lock()
			x.recs[r.id].pstart, x.recs[r.id].plen = pstart, plen
			mu.Unlock()
		}
		close(done)
	}()

	start := time.Now()
	mb := func(n int64) float64 { return float64(n) / (1 << 20) }
	status := func() string {
		secs := time.Since(start).Seconds()
		return fmt.Sprintf("%s: read %.0f/%.0f MiB, dedup %.0f MiB, new %.0f MiB -> %.0f MiB stored, %.0f MiB/s",
			name, mb(nRead.Load()), mb(size), mb(nHit.Load()), mb(nNew.Load()), mb(nOut.Load()), mb(nRead.Load())/secs)
	}
	tick := time.NewTicker(2 * time.Second)
	go func() {
		wg.Wait()
		close(results)
	}()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-tick.C:
			fmt.Fprintln(os.Stderr, status())
		}
	}
	tick.Stop()
	if firstErr != nil {
		return firstErr
	}
	if nRead.Load() != size {
		return fmt.Errorf("read %d bytes, expected %d (file changed while reading?)", nRead.Load(), size)
	}
	fl.size = uint64(size)
	x.files = append(x.files, fl)
	fmt.Fprintf(os.Stderr, "%s in %.1fs\n", status(), time.Since(start).Seconds())
	return nil
}

// ls prints each file with how much of it is shared with earlier files.
func ls(w io.Writer, idxPath string) error {
	x, err := loadIndex(idxPath)
	if err != nil {
		return err
	}
	seen := make([]bool, len(x.recs))
	var stored, unique uint64
	fmt.Fprintf(w, "%-32s %14s %9s %14s %9s %14s\n", "NAME", "SIZE", "CHUNKS", "SHARED", "SHRD-CHK", "NEW-STORED")
	for _, f := range x.files {
		var shared, sharedN, newStored uint64
		mine := map[uint32]bool{}
		for _, id := range f.chunks {
			if seen[id] {
				shared += uint64(x.recs[id].llen)
				sharedN++
			} else if !mine[id] {
				mine[id] = true
				newStored += uint64(x.recs[id].plen &^ rawFlag)
			}
		}
		for id := range mine {
			seen[id] = true
		}
		fmt.Fprintf(w, "%-32s %14d %9d %14d %9d %14d\n", f.name, f.size, len(f.chunks), shared, sharedN, newStored)
	}
	for _, r := range x.recs {
		stored += uint64(r.plen &^ rawFlag)
		unique += uint64(r.llen)
	}
	fmt.Fprintf(w, "%d files, %d unique chunks, %d unique bytes stored in %d bytes, image %d bytes\n",
		len(x.files), len(x.recs), unique, stored, x.blocks*blkSize)
	return nil
}
