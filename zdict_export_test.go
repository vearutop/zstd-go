package zstd

// DarwinHeapsortCalls is the number of heapsort fallbacks of the Darwin qsort emulation so far.
func DarwinHeapsortCalls() int { return darwinHeapsortCalls }

// CoverSortForTest returns COVER's sorted dmer suffix array of samples (COVER_ctx_init's stableSort).
func CoverSortForTest(samples []byte, d uint32, mode QsortMode) []uint32 {
	ctx := &coverCtx{samples: samples, d: d, qsort: mode}
	ctx.suffixSize = len(samples) - int(max(d, 8)) + 1
	ctx.suffix = make([]uint32, ctx.suffixSize)
	for i := range ctx.suffix {
		ctx.suffix[i] = uint32(i)
	}
	ctx.stableSort()
	return ctx.suffix
}
