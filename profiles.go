package zstd

// Parameter presets for Encoder.SetParams and Writer.Params, each close to a klauspost/compress encoder level in speed and
// compression on log-like text (ProfileFastest ~ SpeedFastest, ProfileDefault ~ SpeedDefault, ProfileBestCompression ~
// SpeedBestCompression). Every preset is a plain libzstd configuration: frames are byte-identical to
// ZSTD_compress2 with the same parameters (the tests check it against libzstd), only the choice of parameters is ours. klauspost's
// levels reach further back than libzstd's defaults (4-8 MiB windows, large hash tables), which is most of the difference.
//
// Measured on three inputs of real log lines (8 MB, 59 MB, 5.4 MB; BENCHMARKS.md) against klauspost v1.20.3, single goroutine,
// with about 15% noise between runs:
//
//	preset                  size vs klauspost          speed vs klauspost
//	ProfileFastest          3-8% smaller               0.7-1.0x
//	ProfileDefault          1.5-5.6% smaller           0.75-1.0x
//	ProfileBestCompression  1.5% smaller to 3% larger  1.0-1.5x
//
// There is no counterpart of klauspost's SpeedBetterCompression: its two-table matcher runs at 385-620 MB/s at a ratio where
// libzstd's greedy matcher does 120-215 MB/s, and the port's lazy ProfileBestCompression is only 15-30% slower than greedy
// for 4-5% smaller output.
//
// Tables cost memory per Encoder (4 bytes per entry): ProfileFastest 0.5 MiB, ProfileDefault 1 MiB, ProfileBestCompression about 20 MiB.

// ProfileFastest: libzstd level 1 (the "fast" strategy) with a 4 MiB window and a 128K-entry hash table instead of the
// level's 512 KiB and 16K entries. klauspost's SpeedFastest looks that far back, plain level 1 does not, which cost 6% of the
// size on real logs. A 64 KiB or 32 KiB maxBlockSize (klauspost's block size) does not help.
func ProfileFastest() Params {
	return Params{Level: 1, WindowLog: 22, HashLog: 17}
}

// ProfileDefault: the same fast strategy with an 8 MiB window and a 256K-entry hash table.
func ProfileDefault() Params {
	return Params{Level: 1, WindowLog: 23, HashLog: 18}
}

// ProfileBestCompression: lazy matching with a 6-byte minimum match, search depth 16, an 8 MiB window and large tables.
func ProfileBestCompression() Params {
	return Params{Level: 3, Strategy: 4, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6}
}
