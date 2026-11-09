package zstd

// Parameter presets for Encoder.SetParams and Writer.Params, named after the klauspost/compress encoder level they correspond to
// in speed and compression on log-like text. Every preset is a plain libzstd configuration: frames are byte-identical to
// ZSTD_compress2 with the same parameters (the tests check it against libzstd), only the choice of parameters is ours. klauspost's
// levels reach further back than libzstd's defaults (4-8 MiB windows, large hash tables), which is most of the difference.
//
// Measured on three inputs of real log lines (8 MB, 59 MB, 5.4 MB; BENCHMARKS.md) against klauspost v1.20.3, single goroutine,
// with about 15% noise between runs:
//
//	preset                 size vs klauspost         speed vs klauspost
//	ProfileSpeedFastest    3-8% smaller              0.7-1.0x
//	ProfileSpeedDefault    1.5-5.6% smaller          0.75-1.0x
//	ProfileSpeedBetter     0.5% smaller to 6% larger 0.3-0.35x (no libzstd configuration reaches its speed at this ratio)
//	ProfileSpeedBest       1.5% smaller to 3% larger 1.0-1.5x
//
// Tables cost memory per Encoder (4 bytes per entry): Fastest 0.5 MiB, Default 1 MiB, Better and Best about 20 MiB.

// ProfileSpeedFastest: libzstd level 1 (the "fast" strategy) with a 4 MiB window and a 128K-entry hash table instead of the
// level's 512 KiB and 16K entries. klauspost's SpeedFastest looks that far back, plain level 1 does not, which cost 6% of the
// size on real logs. A 64 KiB or 32 KiB maxBlockSize (klauspost's block size) does not help.
func ProfileSpeedFastest() Params {
	return Params{Level: 1, WindowLog: 22, HashLog: 17}
}

// ProfileSpeedDefault: the same fast strategy with an 8 MiB window and a 256K-entry hash table.
func ProfileSpeedDefault() Params {
	return Params{Level: 1, WindowLog: 23, HashLog: 18}
}

// ProfileSpeedBetter: greedy matching with a 6-byte minimum match, an 8 MiB window and large tables. On log text a longer minimum
// match is worth more than the extra short matches. It reaches SpeedBetter's compression ratio on some inputs and is up to 6%
// larger on others, at about a third of its speed: libzstd's greedy and lazy matchers run at about 100-230 MB/s here.
func ProfileSpeedBetter() Params {
	return Params{Level: 3, Strategy: 3, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6}
}

// ProfileSpeedBest: lazy matching with a 6-byte minimum match, search depth 16, an 8 MiB window and large tables.
func ProfileSpeedBest() Params {
	return Params{Level: 3, Strategy: 4, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6}
}
