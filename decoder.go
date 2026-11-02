package zstd

import "github.com/klauspost/compress/zstd"

// Decoder decompresses whole frames (single-shot, no goroutines); safe for concurrent use.
// ponytail: wraps klauspost's DecodeAll, which matches libzstd speed per #341; own decoder only if that changes.
type Decoder struct{ d *zstd.Decoder }

// NewDecoder returns a decoder that knows the given dictionaries (as passed to LoadDict).
func NewDecoder(dicts ...*Dict) (*Decoder, error) {
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1)}
	for _, d := range dicts {
		if d.id != 0 {
			opts = append(opts, zstd.WithDecoderDicts(d.raw))
		}
	}
	d, err := zstd.NewReader(nil, opts...)
	return &Decoder{d}, err
}

// DecodeAll appends the decompressed frame(s) to dst.
func (d *Decoder) DecodeAll(frame, dst []byte) ([]byte, error) { return d.d.DecodeAll(frame, dst) }
