package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMicroLZMARoundTrip(t *testing.T) {
	src := bytes.Repeat([]byte("erofs dedup round trip "), 20000)
	enc, err := newEncoder(9, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.close()
	dst := make([]byte, len(src))
	n, ok, err := enc.compress(dst, src)
	if err != nil || !ok {
		t.Fatalf("compress: n=%d ok=%v err=%v", n, ok, err)
	}
	if dst[0] == 0 {
		t.Fatal("first compressed byte is 0 (readers treat it as padding)")
	}
	out, err := decompress(dst[:n], len(src), 1<<20)
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("round trip failed: %v", err)
	}
	// Incompressible input must report !ok, not an error.
	rnd := make([]byte, 100000)
	rand.New(rand.NewSource(1)).Read(rnd)
	if _, ok, err := enc.compress(make([]byte, len(rnd)-4096), rnd); ok || err != nil {
		t.Fatalf("random data: ok=%v err=%v", ok, err)
	}
}

func chunkHashes(t *testing.T, data []byte, min, avg, max int) [][32]byte {
	var hs [][32]byte
	total := 0
	err := split(bytes.NewReader(data), min, avg, max, func(c []byte) error {
		if len(c) > max || (len(c) < min && total+len(c) != len(data)) {
			t.Fatalf("chunk size %d out of bounds", len(c))
		}
		total += len(c)
		hs = append(hs, sha256.Sum256(c))
		return nil
	})
	if err != nil || total != len(data) {
		t.Fatalf("split: err=%v total=%d", err, total)
	}
	return hs
}

func TestCDC(t *testing.T) {
	// Pinned: chunk boundaries (and so cross-run dedup) depend on this table.
	if gear[0] != 0x69b543191a3c3fcf || gear[255] != 0x2504c2031df63e9 {
		t.Fatal("gear table changed: existing images would stop deduplicating")
	}
	data := make([]byte, 16<<20)
	rand.New(rand.NewSource(2)).Read(data)
	a := chunkHashes(t, data, 64<<10, 128<<10, 256<<10)
	b := chunkHashes(t, data, 64<<10, 128<<10, 256<<10)
	if len(a) != len(b) {
		t.Fatal("not deterministic")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("not deterministic")
		}
	}
	if avg := len(data) / len(a); avg < 100<<10 || avg > 160<<10 {
		t.Errorf("average chunk %d, want ~128K", avg)
	}
	// An insert near the start must not shift later boundaries.
	shifted := append(append([]byte("inserted bytes"), data[:1000]...), data[1000:]...)
	common := map[[32]byte]bool{}
	for _, h := range a {
		common[h] = true
	}
	same := 0
	for _, h := range chunkHashes(t, shifted, 64<<10, 128<<10, 256<<10) {
		if common[h] {
			same++
		}
	}
	if same < len(a)-3 {
		t.Errorf("only %d of %d chunks survived a small insert", same, len(a))
	}
}

func size(t *testing.T, p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// sample returns ~24 MiB with all chunk kinds: random (raw), zeros (holes),
// text (LZMA) and a repeated block (dedup within the file).
func sample(r *rand.Rand) []byte {
	rnd := func(n int) []byte { b := make([]byte, n); r.Read(b); return b }
	words := strings.Fields("the quick brown fox jumps over lazy dog erofs block device image backup")
	var text bytes.Buffer
	for text.Len() < 6<<20 {
		text.WriteString(words[r.Intn(len(words))])
		text.WriteByte(" \n"[r.Intn(2)])
	}
	rep := rnd(1 << 20)
	var v1 []byte
	v1 = append(v1, rnd(4<<20)...)
	v1 = append(v1, make([]byte, 6<<20)...) // holes
	v1 = append(v1, text.Bytes()...)
	for range 4 {
		v1 = append(v1, rep...)
	}
	return append(v1, rnd(4<<20)...)
}

var testOpts = opts{jobs: 4, min: 64 << 10, avg: 128 << 10, max: 256 << 10, preset: 6, dict: 1 << 20}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	r := rand.New(rand.NewSource(3))
	rnd := func(n int) []byte { b := make([]byte, n); r.Read(b); return b }
	v1 := sample(r)
	v2 := bytes.Clone(v1)
	for range 20 {
		off := r.Intn(len(v2)/4096) * 4096
		copy(v2[off:], rnd(4096))
	}
	at := 12 << 20
	v2 = append(v2[:at:at], append(rnd(100<<10), v2[at:]...)...)

	v3 := bytes.Clone(v2) // for the crash test
	copy(v3[5<<20:], rnd(8192))

	files := map[string][]byte{"v1": v1, "v2": v2, "v3": v3}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(dir, "img.erofs")
	o := testOpts
	o.index = img + ".idx"
	src := func(n string) []string { return []string{filepath.Join(dir, n)} }

	if err := run(true, img, o, src("v1")); err != nil {
		t.Fatal(err)
	}
	s1 := size(t, img)
	if err := run(false, img, o, src("v2")); err != nil {
		t.Fatal(err)
	}
	s2 := size(t, img)
	if g := s2 - s1; g > int64(len(v2))/4 {
		t.Errorf("adding v2 grew the image by %d bytes (len %d)", g, len(v2))
	}
	o2 := o
	o2.name = "v2-again"
	files["v2-again"] = v2
	if err := run(false, img, o2, src("v2")); err != nil {
		t.Fatal(err)
	}
	s3 := size(t, img)
	if g, meta := s3-s2, int64(2*4096+96+32*(len(v2)/o.min+1)); g > 64<<10+meta {
		t.Errorf("re-adding v2 grew the image by %d bytes", g)
	}
	if err := run(false, img, o2, src("v1")); err == nil {
		t.Error("duplicate name accepted")
	}
	t.Logf("v1 %d -> img %d, +v2 %d, +v2 again %d", len(v1), s1, s2-s1, s3-s2)

	// Crash mid-add: garbage appended after the committed end, sidecar old.
	// The image must still be valid as it is.
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(rnd(1<<20), s3)
	f.Close()
	fsck(t, img, "")
	if err := run(false, img, o, src("v3")); err != nil {
		t.Fatal(err)
	}

	// A stale sidecar must be refused rather than truncating newer data.
	idx, _ := os.ReadFile(o.index)
	o2.name = "v1-again"
	if err := run(false, img, o2, src("v1")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(o.index+".stale", idx, 0o644)
	o3 := o
	o3.index, o3.name = o.index+".stale", "x"
	if err := run(false, img, o3, src("v1")); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("stale index not refused: %v", err)
	}
	files["v1-again"] = v1

	// Lost sidecar: reindex rebuilds an equivalent one, and add still dedups.
	os.Rename(o.index, o.index+".orig")
	if err := reindex(img, o.index, 3); err != nil {
		t.Fatal(err)
	}
	if err := reindex(img, o.index, 3); err == nil {
		t.Error("reindex overwrote an existing index")
	}
	checkReindexed(t, o.index+".orig", o.index)
	s4 := size(t, img)
	o2.name = "v2-reindexed"
	if err := run(false, img, o2, src("v2")); err != nil {
		t.Fatal(err)
	}
	if g, meta := size(t, img)-s4, int64(2*4096+96+32*(len(v2)/o.min+1)); g > 64<<10+meta {
		t.Errorf("adding v2 after reindex grew the image by %d bytes", g)
	}
	files["v2-reindexed"] = v2

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not installed; image not verified")
	}
	checkFiles(t, img, files)
	b, err := exec.Command("dump.erofs", "-s", img).CombinedOutput()
	if err != nil || !bytes.Contains(b, []byte("48bit")) || !bytes.Contains(b, []byte("lzma")) {
		t.Errorf("dump.erofs -s: %v\n%s", err, b)
	}
	b, err = exec.Command("dump.erofs", "--path=/v1", "-e", img).CombinedOutput()
	if err != nil || !bytes.Contains(b, []byte("Ext:")) {
		t.Errorf("dump.erofs -e: %v\n%s", err, b)
	}
}

func TestDirs(t *testing.T) {
	dir := t.TempDir()
	big := sample(rand.New(rand.NewSource(4)))
	src := func(p string) string { return filepath.Join(dir, "src", p) }
	want := map[string][]byte{} // image path -> content, nil for a directory
	put := func(p, img string, b []byte) {
		os.MkdirAll(filepath.Dir(src(p)), 0o755)
		if err := os.WriteFile(src(p), b, 0o644); err != nil {
			t.Fatal(err)
		}
		want[img] = b
	}
	put("top/a.bin", "top/a.bin", big)
	put("top/same.bin", "top/same.bin", big[:1<<20])
	put("top/sub/same.bin", "top/sub/same.bin", big[10<<20:12<<20])
	put("top/sub/deep/c.bin", "top/sub/deep/c.bin", big[:3<<20])
	put("one.bin", "a/b/c.img", big[8<<20:12<<20]) // fsck --extract truncates a trailing hole
	put("dot/top/new.bin", "top/new.bin", big[12<<20:13<<20])
	put("dot/z.bin", "z.bin", []byte("z"))
	put("again/top/a.bin", "", nil)
	delete(want, "")
	os.MkdirAll(src("top/empty"), 0o755)
	if err := os.Symlink("a.bin", src("top/link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"top", "top/sub", "top/sub/deep", "top/empty", "a", "a/b"} {
		want[d] = nil
	}

	img := filepath.Join(dir, "img.erofs")
	o := testOpts
	o.index = img + ".idx"
	named := func(n string) opts { o2 := o; o2.name = n; return o2 }
	if err := run(true, img, o, []string{src("top")}); err != nil {
		t.Fatal(err)
	}
	if err := run(false, img, named("/a//b/c.img"), []string{src("one.bin")}); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(src("dot"))
	err := run(false, img, o, []string{"."}) // contents into the root, merging /top
	os.Chdir(wd)
	if err != nil {
		t.Fatal(err)
	}

	// Collisions are refused before anything is written.
	s0, idx0 := size(t, img), must(os.ReadFile(o.index))
	for _, c := range []struct {
		o    opts
		path string
	}{
		{named("top"), "one.bin"},         // file vs existing dir
		{named("a/b/c.img"), "again/top"}, // dir vs existing file
		{named("a/b/c.img"), "one.bin"},   // same path
		{named("a/b/c.img/x"), "one.bin"}, // parent is a file
		{named("a/../b"), "one.bin"},      // bad name
		{o, "again/top"},                  // merges /top, then top/a.bin collides
	} {
		if err := run(false, img, c.o, []string{src(c.path)}); err == nil {
			t.Errorf("-name %q %s accepted", c.o.name, c.path)
		}
	}
	if size(t, img) != s0 || !bytes.Equal(must(os.ReadFile(o.index)), idx0) {
		t.Fatal("a refused add changed the image or the index")
	}

	os.Rename(o.index, o.index+".orig")
	if err := reindex(img, o.index, 3); err != nil {
		t.Fatal(err)
	}
	checkReindexed(t, o.index+".orig", o.index)

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not installed; image not verified")
	}
	out := filepath.Join(dir, "out")
	if b, err := exec.Command("fsck.erofs", "--extract="+out, img).CombinedOutput(); err != nil {
		t.Fatalf("fsck: %v\n%s", err, b)
	}
	got := map[string][]byte{}
	filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if rel, _ := filepath.Rel(out, p); err == nil && rel != "." {
			got[rel] = nil
			if !d.IsDir() {
				got[rel] = must(os.ReadFile(p))
			}
		}
		return err
	})
	if !maps.EqualFunc(got, want, bytes.Equal) || got["top/empty"] != nil {
		t.Errorf("extracted tree differs: got %v", slices.Sorted(maps.Keys(got)))
	}
	b, err := exec.Command("dump.erofs", "--ls", "--path=/a/b", img).CombinedOutput()
	if err != nil || !bytes.Contains(b, []byte("c.img")) {
		t.Errorf("dump.erofs --ls: %v\n%s", err, b)
	}
}

// fsck runs fsck.erofs on img, extracting to out if out != "". Without
// fsck.erofs it does nothing (TestEndToEnd and TestDirs then skip at the end).
func fsck(t *testing.T, img, out string) {
	t.Helper()
	args := []string{img}
	if out != "" {
		args = []string{"--extract=" + out, img}
	}
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}
	if b, err := exec.Command("fsck.erofs", args...).CombinedOutput(); err != nil {
		t.Fatalf("fsck: %v\n%s", err, b)
	}
}

// checkFiles extracts img and compares the files (root-level names) with want;
// names absent from want must not be there.
func checkFiles(t *testing.T, img string, want map[string][]byte) {
	t.Helper()
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Log("fsck.erofs not installed; files not verified")
		return
	}
	out := t.TempDir()
	fsck(t, img, out)
	got := map[string][]byte{}
	for _, e := range must(os.ReadDir(out)) {
		got[e.Name()] = must(os.ReadFile(filepath.Join(out, e.Name())))
	}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("extracted files differ: got %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}

func zeroBlock0(t *testing.T, img string) {
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(make([]byte, blkSize), 0); err != nil {
		t.Fatal(err)
	}
}

// TestRecovery: superblock copy + fixsb, and a foreign sidecar.
func TestRecovery(t *testing.T) {
	dir := t.TempDir()
	r := rand.New(rand.NewSource(5))
	rnd := func(n int) []byte { b := make([]byte, n); r.Read(b); return b }
	files := map[string][]byte{"a": sample(r), "b": rnd(3 << 20), "c": rnd(2 << 20)}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(dir, "img.erofs")
	o := testOpts
	o.index = img + ".idx"
	noIdx := o
	noIdx.index = filepath.Join(dir, "missing.idx")
	src := func(n string) []string { return []string{filepath.Join(dir, n)} }
	if err := run(true, img, o, src("a")); err != nil {
		t.Fatal(err)
	}
	if err := run(false, img, o, src("b")); err != nil {
		t.Fatal(err)
	}

	// (a) The last block is a copy of block 0.
	trailer := func() {
		t.Helper()
		b := must(os.ReadFile(img))
		if !bytes.Equal(b[len(b)-blkSize:], b[:blkSize]) {
			t.Error("last block differs from block 0")
		}
	}
	trailer()

	// (b) Zeroed block 0: add is refused untouched, fixsb restores it.
	zeroBlock0(t, img)
	before := must(os.ReadFile(img))
	if err := run(false, img, o, nil); err == nil || !strings.Contains(err.Error(), "fixsb") {
		t.Errorf("add on a zeroed superblock not refused: %v", err)
	}
	if !bytes.Equal(must(os.ReadFile(img)), before) {
		t.Error("a refused add changed the image")
	}
	if err := fixsb(img, noIdx); err != nil {
		t.Fatal(err)
	}
	ab := map[string][]byte{"a": files["a"], "b": files["b"]}
	checkFiles(t, img, ab)
	if err := run(false, img, o, src("c")); err != nil {
		t.Fatal(err)
	}
	trailer()
	checkFiles(t, img, files)

	// (c) fixsb refuses a garbage last block, and a valid copy at the wrong offset.
	f := must(os.OpenFile(img, os.O_RDWR, 0))
	sz := size(t, img)
	b0 := make([]byte, blkSize)
	f.ReadAt(b0, 0)
	for _, blk := range []struct {
		data []byte
		off  int64
	}{{rnd(blkSize), sz - blkSize}, {b0, sz}} {
		f.WriteAt(blk.data, blk.off)
		before := must(os.ReadFile(img))
		if err := fixsb(img, noIdx); err == nil {
			t.Error("fixsb accepted a bad superblock copy")
		}
		if !bytes.Equal(must(os.ReadFile(img)), before) {
			t.Error("a refused fixsb changed the image")
		}
	}
	f.Close()

	// (d) A foreign sidecar is refused by the uuid check, image left alone.
	img2 := filepath.Join(dir, "img2.erofs")
	o2 := testOpts
	o2.index = img2 + ".idx"
	if err := run(true, img2, o2, src("b")); err != nil {
		t.Fatal(err)
	}
	before = must(os.ReadFile(img2))
	if err := run(false, img2, o, nil); err == nil || !strings.Contains(err.Error(), "uuid") {
		t.Errorf("foreign sidecar not refused: %v", err)
	}
	if !bytes.Equal(must(os.ReadFile(img2)), before) {
		t.Error("a refused add changed the image")
	}

	// (e) Block 0 and its copy both bad: fixsb needs a matching index.
	zeroBlock0(t, img)
	f = must(os.OpenFile(img, os.O_RDWR, 0))
	f.WriteAt(rnd(blkSize), size(t, img)-blkSize)
	f.Close()
	before = must(os.ReadFile(img))
	for _, bad := range []opts{noIdx, o2} {
		if err := fixsb(img, bad); err == nil {
			t.Errorf("fixsb with index %s accepted a double failure", bad.index)
		}
		if !bytes.Equal(must(os.ReadFile(img)), before) {
			t.Error("a refused fixsb changed the image")
		}
	}
	if err := fixsb(img, o); err != nil {
		t.Fatal(err)
	}
	trailer()
	checkFiles(t, img, files)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func checkReindexed(t *testing.T, origPath, newPath string) {
	a, err := loadIndex(origPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadIndex(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if a.uuid != b.uuid || a.label != b.label || a.dataEnd != b.dataEnd || a.blocks != b.blocks || a.dict != b.dict {
		t.Fatalf("header differs: %+v vs %+v", *a, *b)
	}
	if !slices.Equal(a.dirs, b.dirs) {
		t.Errorf("dirs differ:\n%v\n%v", a.dirs, b.dirs)
	}
	type chunk struct {
		hash [32]byte
		kind uint32 // 0 hole, 1 LZMA, rawFlag raw
		llen uint32
	}
	kinds := map[uint32]bool{}
	seq := func(x *index, f file) (s []chunk) {
		for _, id := range f.chunks {
			c := x.recs[id]
			k := c.plen & rawFlag
			if c.plen != 0 && k == 0 {
				k = 1 // LZMA
			}
			kinds[k] = true
			s = append(s, chunk{c.hash, k, c.llen})
		}
		return s
	}
	if len(a.files) != len(b.files) {
		t.Fatalf("%d files, want %d", len(b.files), len(a.files))
	}
	for i, fa := range a.files {
		fb := b.files[i]
		if fa.name != fb.name || fa.size != fb.size || fa.mtime != fb.mtime || fa.nsec != fb.nsec ||
			!slices.Equal(seq(a, fa), seq(b, fb)) {
			t.Errorf("file %d (%s) differs", i, fa.name)
		}
	}
	if len(kinds) != 3 {
		t.Errorf("test image lacks a chunk kind (hole/raw/lzma): %v", kinds)
	}
	set := map[rec]bool{}
	for _, c := range a.recs {
		set[c] = true
	}
	for _, c := range b.recs {
		if !set[c] {
			t.Errorf("rebuilt rec %+v not in the original", c)
		}
	}
	if len(a.recs) != len(b.recs) {
		t.Errorf("%d recs, want %d", len(b.recs), len(a.recs))
	}
}
