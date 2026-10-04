package main

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/lzma
#cgo LDFLAGS: -l:liblzma.so.5
#include <stdlib.h>
#include <lzma.h>

// The Go buffers are only referenced for the duration of the call.
static lzma_ret mlz_enc(lzma_stream *s, const lzma_options_lzma *o,
		const uint8_t *in, size_t inlen, uint8_t *out, size_t outlen) {
	lzma_ret r = lzma_microlzma_encoder(s, o);
	if (r != LZMA_OK)
		return r;
	s->next_in = in; s->avail_in = inlen;
	s->next_out = out; s->avail_out = outlen;
	r = lzma_code(s, LZMA_FINISH);
	s->next_in = NULL; s->next_out = NULL;
	return r;
}

static lzma_ret mlz_dec(const uint8_t *in, size_t inlen, uint8_t *out, size_t outlen, uint32_t dict) {
	lzma_stream s = LZMA_STREAM_INIT;
	lzma_ret r = lzma_microlzma_decoder(&s, inlen, outlen, 1, dict);
	if (r != LZMA_OK)
		return r;
	s.next_in = in; s.avail_in = inlen;
	s.next_out = out; s.avail_out = outlen;
	r = lzma_code(&s, LZMA_FINISH);
	if (r == LZMA_STREAM_END && s.total_out != outlen)
		r = LZMA_DATA_ERROR;
	lzma_end(&s);
	return r;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// encoder is one MicroLZMA stream, owned by a single worker goroutine.
type encoder struct {
	s   *C.lzma_stream
	opt *C.lzma_options_lzma
}

func newEncoder(preset int, extreme bool, dict int) (*encoder, error) {
	e := &encoder{
		s:   (*C.lzma_stream)(C.calloc(1, C.sizeof_lzma_stream)),
		opt: (*C.lzma_options_lzma)(C.calloc(1, C.sizeof_lzma_options_lzma)),
	}
	p := C.uint32_t(preset)
	if extreme {
		p |= C.LZMA_PRESET_EXTREME
	}
	if C.lzma_lzma_preset(e.opt, p) != 0 {
		e.close()
		return nil, fmt.Errorf("bad lzma preset %d", preset)
	}
	e.opt.dict_size = C.uint32_t(dict)
	return e, nil
}

// compress MicroLZMA-encodes src into dst. ok is false when the result would
// not be smaller than src (dst too small to hold all of it).
func (e *encoder) compress(dst, src []byte) (n int, ok bool, err error) {
	r := C.mlz_enc(e.s, e.opt, (*C.uint8_t)(unsafe.Pointer(&src[0])), C.size_t(len(src)),
		(*C.uint8_t)(unsafe.Pointer(&dst[0])), C.size_t(len(dst)))
	if r != C.LZMA_STREAM_END {
		return 0, false, fmt.Errorf("lzma encoder error %d", int(r))
	}
	n = int(e.s.total_out)
	return n, int(e.s.total_in) == len(src) && n < len(src), nil
}

func (e *encoder) close() {
	C.lzma_end(e.s)
	C.free(unsafe.Pointer(e.s))
	C.free(unsafe.Pointer(e.opt))
}

// decompress decodes one MicroLZMA pcluster (reindex, tests).
func decompress(src []byte, ulen, dict int) ([]byte, error) {
	out := make([]byte, ulen)
	r := C.mlz_dec((*C.uint8_t)(unsafe.Pointer(&src[0])), C.size_t(len(src)),
		(*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(ulen), C.uint32_t(dict))
	if r != C.LZMA_STREAM_END {
		return nil, fmt.Errorf("lzma decoder error %d", int(r))
	}
	return out, nil
}
