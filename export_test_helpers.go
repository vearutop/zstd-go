package zstd

// ParseHeaderForTest exposes parseHeader to the external test package.
func ParseHeaderForTest(b []byte) (size uint64, n int, err error) {
	h, n, err := parseHeader(b)
	return h.size, n, err
}

// SequencesForTest decodes frame and returns its sequences as (ll, ml, offBase) triples.
func SequencesForTest(d *Decoder, frame []byte) ([][3]int, error) {
	var seqs [][3]int
	seqTrace = func(ll, ml, ob int) { seqs = append(seqs, [3]int{ll, ml, ob}) }
	defer func() { seqTrace = nil }()
	_, err := d.DecodeAll(frame, nil)
	return seqs, err
}

// ContentLenForTest returns the dictionary content length.
func (d *Dict) ContentLenForTest() int { return len(d.content) }

// SetDebugErrsForTest makes corruption errors name their source line.
func SetDebugErrsForTest(on bool) { debugErrs = on }

// DictDecodableForTest reports whether the decoder accepts the dictionary's entropy tables (ZSTD_createDDict succeeds).
func DictDecodableForTest(d *Dict) bool { return dictDecodable(d) == nil }

// ViolatesWindowForTest reports whether decoding frame uses a match offset beyond the frame's declared window size
// (an invalid frame that libzstd's streaming ring buffer treats differently from a sliding window).
func ViolatesWindowForTest(d *Decoder, frame []byte) bool {
	hd, _, err := parseHeader(frame)
	if err != nil {
		return false
	}
	bad := false
	offTrace = func(off, produced int) {
		if uint64(off) > hd.window {
			bad = true
		}
	}
	defer func() { offTrace = nil }()
	d.DecodeAll(frame, nil) //nolint:errcheck // only the offsets seen on the way matter
	return bad
}
