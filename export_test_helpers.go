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
