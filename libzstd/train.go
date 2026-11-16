//go:build cgo

package libzstd

// TrainParams configures TrainDict (libzstd's fastCover trainer). The zero value is libzstd's default: a search over k and d
// with 40 steps.
type TrainParams struct {
	K, D       int     // segment size and dmer size; both non-zero skips the search and runs the trainer once
	F          int     // log of the frequency array size (0: 20)
	Steps      int     // search steps (0: 40)
	NbThreads  int     // parallel search workers (0: 1)
	SplitPoint float64 // share of samples used for training, the rest validates (0: 0.75)
	Accel      int     // speed/accuracy trade-off 1..10 (0: 1, most accurate)
	Level      int     // compression level the dictionary is tuned for (0: 3)
	DictID     uint32  // dictionary id (0: derived from the content)
}

// TrainDict builds a zstd-format dictionary of at most dictSize bytes from samples (ZDICT fastCover). The samples should be
// typical small values; libzstd recommends about 100x as much sample data as dictSize. The result is deterministic.
func TrainDict(samples [][]byte, dictSize int, p TrainParams) ([]byte, error) {
	var flat []byte
	sizes := make([]uintptr, len(samples))
	for i, s := range samples {
		flat = append(flat, s...)
		sizes[i] = uintptr(len(s))
	}
	if len(sizes) == 0 || dictSize <= 0 {
		return nil, errNoSamples
	}
	return trainFastCover(flat, sizes, dictSize, p)
}
