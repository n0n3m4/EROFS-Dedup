package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"slices"
	"time"
)

const (
	blkSize = 4096
	rootNid = 1 // nid 0 left unused

	fmtLZMA = 2 << 28 // z_erofs_extent.plen: (Z_EROFS_COMPRESSION_LZMA + 1) << 28
)

var le = binary.LittleEndian

func roundUp(x, a uint64) uint64 { return (x + a - 1) / a * a }

type dirent struct {
	name string
	nid  uint64
	typ  uint8 // 1 = REG, 2 = DIR
}

// dirBlocks lays out a FLAT_PLAIN directory: per block, 12-byte dirents then
// the names (no NULs). Returns the data (whole blocks) and i_size.
func dirBlocks(ents []dirent) ([]byte, uint64) {
	slices.SortFunc(ents, func(a, b dirent) int {
		if a.name < b.name { // Go string compare is bytewise
			return -1
		}
		return 1
	})
	var out []byte
	var size uint64
	for i := 0; i < len(ents); {
		// how many entries fit in this block
		n, used := 0, 0
		for i+n < len(ents) && used+12+len(ents[i+n].name) <= blkSize {
			used += 12 + len(ents[i+n].name)
			n++
		}
		blk := make([]byte, blkSize)
		nameoff := 12 * n
		for j, e := range ents[i : i+n] {
			d := blk[12*j:]
			le.PutUint64(d, e.nid)
			le.PutUint16(d[8:], uint16(nameoff))
			d[10] = e.typ
			nameoff += copy(blk[nameoff:], e.name)
		}
		size = uint64(len(out)) + uint64(used)
		out = append(out, blk...)
		i += n
	}
	return out, size
}

// extInode encodes a 64-byte erofs_inode_extended.
func extInode(layout uint16, mode uint16, size uint64, u uint64, ino uint32, mtime int64, nsec uint32, nlink uint32) []byte {
	b := make([]byte, 64)
	le.PutUint16(b[0:], 1|layout<<1) // extended inode
	le.PutUint16(b[4:], mode)
	le.PutUint16(b[6:], uint16(u>>32)) // i_nb: startblk_hi / blocks_hi
	le.PutUint64(b[8:], size)
	le.PutUint32(b[16:], uint32(u)) // i_u: startblk_lo / blocks_lo
	le.PutUint32(b[20:], ino)
	le.PutUint64(b[32:], uint64(mtime))
	le.PutUint32(b[40:], nsec)
	le.PutUint32(b[44:], nlink)
	return b
}

// parentDir returns the parent of an image path, "" for the root.
func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// writeMeta writes the directory blocks and all inodes at round_up(x.dataEnd)
// (after everything committed so far: the previous metadata becomes dead space),
// truncates the image to the end of the metadata and returns the image size
// in blocks plus block 0 (superblock), which the caller writes last. No fsync.
func writeMeta(img *os.File, x *index) (uint64, []byte, error) {
	dirBlk := roundUp(x.dataEnd, blkSize) / blkSize
	now := time.Now()

	// Pass 1: inode offsets relative to the metadata area, so they don't depend
	// on the directory blocks before it. A 64-byte inode core is never split
	// across a block boundary. Root first, then the other directories, then the
	// files in x.files order (reindex relies on nid order = append order).
	type dnode struct {
		directory
		nid, blk, size uint64
		nsub           uint32
		ents           []dirent
	}
	nodes := []dnode{{directory: directory{"", now.Unix(), uint32(now.Nanosecond())}}}
	for _, d := range x.dirs {
		nodes = append(nodes, dnode{directory: d})
	}
	type slot struct {
		off uint64
		f   *file
		d   *dnode
	}
	var slots []slot
	off := uint64(rootNid * 32)
	byPath := map[string]*dnode{}
	for i := range nodes {
		if off%blkSize == blkSize-32 {
			off += 32
		}
		n := &nodes[i]
		n.nid = off / 32
		byPath[n.path] = n
		slots = append(slots, slot{off, nil, n})
		off += 64
	}
	for i := range x.files {
		f := &x.files[i]
		if off%blkSize == blkSize-32 {
			off += 32
		}
		slots = append(slots, slot{off, f, nil})
		p := byPath[parentDir(f.name)]
		p.ents = append(p.ents, dirent{path.Base(f.name), off / 32, 1})
		off += 64
		if len(f.chunks) > 0 {
			off += 32 + 32*uint64(len(f.chunks)) // map header @+64, records @+96
		}
	}
	for i := range nodes {
		n := &nodes[i]
		p := byPath[parentDir(n.path)] // the root is its own parent
		if n.path != "" {
			p.ents = append(p.ents, dirent{path.Base(n.path), n.nid, 2})
			p.nsub++
		}
		n.ents = append(n.ents, dirent{".", n.nid, 2}, dirent{"..", p.nid, 2})
	}

	// Pass 2: directory blocks (root first) right before the metadata area.
	var dirs []byte
	for i := range nodes {
		n := &nodes[i]
		b, size := dirBlocks(n.ents)
		n.blk, n.size = dirBlk+uint64(len(dirs))/blkSize, size
		dirs = append(dirs, b...)
	}
	metaBlk := dirBlk + uint64(len(dirs))/blkSize
	meta := make([]byte, roundUp(off, blkSize))
	for i, s := range slots {
		f := s.f
		var b []byte
		if s.d != nil {
			b = extInode(0, 0o40755, s.d.size, s.d.blk, uint32(i+1), s.d.mtime, s.d.nsec, 2+s.d.nsub)
		} else if len(f.chunks) == 0 {
			b = extInode(0, 0o100644, 0, 0, uint32(i+1), f.mtime, f.nsec, 1)
		} else {
			var pbytes, lstart uint64
			r := meta[s.off+64:]
			le.PutUint32(r, uint32(len(f.chunks)))         // h_extents_lo
			le.PutUint16(r[4:], 0x0007)                    // h_advise: EXTENTS | recsz 32
			le.PutUint16(r[6:], uint16(len(f.chunks)>>32)) // h_extents_hi
			for j, id := range f.chunks {
				c := x.recs[id]
				plen := c.plen &^ rawFlag
				switch {
				case c.plen == 0: // hole
				case c.plen&rawFlag != 0:
				default:
					plen |= fmtLZMA
				}
				e := r[32+32*j:]
				le.PutUint32(e, plen)
				le.PutUint64(e[4:], c.pstart) // pstart_lo, pstart_hi
				le.PutUint64(e[12:], lstart)  // lstart_lo, lstart_hi
				lstart += uint64(c.llen)
				pbytes += uint64(c.plen &^ rawFlag)
			}
			if lstart != f.size {
				return 0, nil, fmt.Errorf("%s: chunks cover %d bytes, size is %d", f.name, lstart, f.size)
			}
			b = extInode(1, 0o100644, f.size, roundUp(pbytes, blkSize)/blkSize, uint32(i+1), f.mtime, f.nsec, 1)
		}
		copy(meta[s.off:], b)
	}
	if _, err := img.WriteAt(append(dirs, meta...), int64(dirBlk*blkSize)); err != nil {
		return 0, nil, err
	}
	blocks := metaBlk + uint64(len(meta))/blkSize
	// ponytail: images capped at 16 TiB (32-bit blocks_lo/meta_blkaddr). Beyond
	// that needs blocks_hi + rootnid_8b, whose decoding differs between
	// erofs-utils 1.9 (lo<<32|hi) and HEAD (lo|hi<<32); verify the kernel first.
	if blocks > 1<<32-1 {
		return 0, nil, fmt.Errorf("image would exceed 16 TiB")
	}

	// Block 0: superblock at 1024, LZMA cfg right after the 128-byte sb.
	b0 := make([]byte, blkSize)
	sb := b0[1024:]
	le.PutUint32(sb[0:], 0xE0F5E1E2)
	le.PutUint32(sb[8:], 0x3)      // compat: SB_CHKSUM | MTIME
	sb[12] = 12                    // blkszbits
	le.PutUint16(sb[14:], rootNid) // rootnid_2b (rootnid_8b stays 0, see above)
	le.PutUint64(sb[16:], uint64(len(slots)))
	le.PutUint32(sb[36:], uint32(blocks))
	le.PutUint32(sb[40:], uint32(metaBlk))
	copy(sb[48:], x.uuid[:])
	copy(sb[64:], x.label[:])
	le.PutUint32(sb[80:], 0x83) // incompat: LZ4_0PADDING | COMPR_CFGS | 48BIT
	le.PutUint16(sb[84:], 1<<1) // available_compr_algs: LZMA
	le.PutUint32(sb[108:], uint32(now.Unix()))
	le.PutUint16(sb[128:], 14)     // cfg record size
	le.PutUint32(sb[130:], x.dict) // z_erofs_lzma_cfgs.dict_size, format 0
	le.PutUint32(sb[4:], sbChecksum(sb))
	return blocks, b0, img.Truncate(int64(blocks * blkSize))
}

// sbChecksum is the EROFS superblock checksum of sb (block 0 from offset 1024),
// computed with the checksum field taken as 0.
func sbChecksum(sb []byte) uint32 {
	c := bytes.Clone(sb[:blkSize-1024])
	le.PutUint32(c[4:], 0)
	return ^crc32.Checksum(c, crc32.MakeTable(crc32.Castagnoli))
}
