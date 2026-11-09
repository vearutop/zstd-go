package zstd

// Parameter presets for Encoder.SetParams and Writer.Params. Every preset is a plain libzstd configuration: frames are
// byte-identical to ZSTD_compress2 with the same parameters.

// ProfileSpeedFastest is the port's counterpart of klauspost/compress's SpeedFastest, for high-volume text such as logs:
// libzstd level 1 (the "fast" strategy) with a 4 MiB window and a 128K-entry hash table instead of the level's 512 KiB and
// 16K entries. klauspost's SpeedFastest looks that far back, plain level 1 does not, which cost 6% of the size on real logs.
// Measured on log lines: the same speed as SpeedFastest and 5-8% smaller output (BENCHMARKS.md). Smaller blocks do not help.
func ProfileSpeedFastest() Params {
	return Params{Level: 1, WindowLog: 22, HashLog: 17}
}
