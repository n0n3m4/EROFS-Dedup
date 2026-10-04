# erofs-dedup

Dedup-first EROFS image builder for backing up many versions of large disk images
into one mountable image. Chunks the input (content-defined, gear hash), SHA-256
dedups **before** compressing, and stores each unique chunk once as an EROFS
encoded extent (MicroLZMA, 48-bit layout). Files share extents freely.

    erofs-dedup create IMAGE [opts] FILE...      # new image + IMAGE.idx
    erofs-dedup add    IMAGE [opts] [FILE...]    # append, dedup against everything already there
    erofs-dedup ls     IMAGE                     # files (full paths) and how much they share
    erofs-dedup reindex IMAGE [-index PATH] [-j N]  # rebuild a lost IMAGE.idx from the image
    erofs-dedup fixsb  IMAGE [-index PATH]       # restore a torn/zeroed block 0 from its copy

    erofs-dedup add backup.erofs -name 2026-10/sda.img /dev/sda   # -> /2026-10/sda.img

FILE can be a regular file, a block device or a directory. Options go **before** the files.

* A file lands at `/<basename>`. A directory `d` is copied recursively to `/<basename of d>/...`
  like `cp -r d /`; `.` puts the current directory's contents into the root. Empty
  directories are kept; symlinks and special files inside a directory are skipped with a
  warning (a symlink given as FILE is followed).
* `-name PATH` (exactly one FILE) is the full path inside the image; missing parent
  directories are created. For a directory FILE, its contents go under PATH. Components
  are <= 255 bytes, no `..`.
* Every path must be new, except directories, which merge (adding into an existing
  `/2026-10/` is fine). Any collision is refused before anything is written.
* Directories are 0755 root:root with the source's mtime (commit time for ones created by
  `-name`); files are 0644 root:root with the source's mtime.
`-j N` workers, `-chunk-min/-chunk-avg/-chunk-max` (256K/512K/983040),
`-lzma-preset 0..9`, `-lzma-extreme`, `-lzma-dict` (1 MiB; presets 6..9 are
equivalent with this dict), `-name`, `-uuid`, `-label`, `-index`.

Build: `go build` (Go 1.24, cgo). Links against the system `liblzma.so.5`
(xz >= 5.4); headers are vendored in `third_party/lzma` (0BSD), no `-dev` package needed.

## How it works

* One sequential read of each input, append-only writes to the image, no re-reads.
  Match = equal SHA-256 (no byte comparison). The index lives in RAM (~50 B per unique chunk).
* Only chunks never seen before (in this run or any previous one) go through LZMA, so
  adding a mostly-unchanged image costs CPU for its new chunks only.
* Chunk kinds: LZMA-compressed (if it saves >= 1 block), raw, hole (all zeros: no data).
* Layout: block 0 (superblock) | data chunks | metadata (directories, inodes, extent
  lists) | 4 KiB superblock copy. The copy lies outside the block count in the
  superblock, so EROFS readers ignore it.
* `IMAGE.idx` is the sidecar index: chunk hashes/locations, the directories and per-file
  chunk lists. Indexes from before directory support (`EROFSDD1`) still load.
  It is needed for `add` and `ls`; the image itself mounts without it.
* `add` appends new chunks and then a complete new copy of the metadata after the
  committed end of the image, and rewrites block 0 last. Commit order: data + metadata
  + superblock copy fsync, sidecar (tmp+rename), block 0, fsync. The old metadata stays
  behind as dead space (about 32 B per extent plus 64 B per inode, rewritten on every
  `add`; small next to the data). Do not run `add` on a mounted image: the kernel
  caches the old superblock.
* `add` refuses an image without a valid superblock, with another uuid, newer than the
  sidecar, or whose last stored chunk doesn't match the sidecar.

### Recovery

The image is the only source of truth. `IMAGE.idx` is a cache for `add` (chunk hashes
and file chunk lists) and can always be rebuilt with `reindex`, at the cost of one full
sequential read + decompress of the image.

* Crash during `add`: the image is the previous one, intact and mountable (the appended
  tail is ignored, the next `add` truncates it away). Re-run the `add`, or `add IMAGE`
  with no FILE if the sidecar was already updated.
* Torn or zeroed block 0: `erofs-dedup fixsb IMAGE` restores it from the 4 KiB copy
  kept right after the metadata (written before block 0 on every commit). This is the one
  failure a plain EROFS image cannot recover from. `add` refuses such an image until then.
* Block 0 and its copy both damaged: `erofs-dedup fixsb IMAGE -index IMAGE.idx` rebuilds
  the superblock (and rewrites the metadata) from the index after verifying the index
  against the image by content; only use a current index.
* Lost or corrupt index: `reindex` (refuses to overwrite an existing index; it reads what
  the superblock describes, so after an interrupted `add` it rebuilds the previous
  generation). An index from another image is refused by `add`.

## Verification

Images are checked with erofs-utils 1.9: `fsck.erofs --extract=DIR IMAGE` (byte-identical
files), `dump.erofs -s/-e`, and an `erofsfuse` mount. Caveat: `fsck.erofs --extract` (1.9)
seeks over holes and never extends the output file, so a file whose tail is all zeros is
extracted short (fsck/main.c `lseek` in the unmapped branch). The image is correct: an
`erofsfuse` mount and the kernel report the full size and read zeros there. Use
`erofsfuse IMAGE DIR` + `cmp` to verify disk images that end in zeros. Mounting with the kernel driver
needs Linux >= 6.15 (encoded extents); not tested here, see the report for details.
