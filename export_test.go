package zstd

func init() { strictHufDefault = true } // the existing differential tests expect libzstd's exact behavior

// SetStrictHufDefaultForTest switches the package-wide strict default (tests of the default fast path turn it off).
func SetStrictHufDefaultForTest(v bool) { strictHufDefault = v }

// SetTrimSlackDivForTest lets the external benchmarks change the streaming history slack.
func SetTrimSlackDivForTest(d int) { trimSlackDiv = d }

// SetTrimFloorForTest sets the minimum history size before shifting.
func SetTrimFloorForTest(n int) { trimFloor = n }

func SetSeqAsmBackoffForTest(n int) { seqAsmBackoff = n }
