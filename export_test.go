package zstd

func init() { strictHufDefault = true } // the existing differential tests expect libzstd's exact behavior

// SetStrictHufDefaultForTest switches the package-wide strict default (tests of the default fast path turn it off).
func SetStrictHufDefaultForTest(v bool) { strictHufDefault = v }

// SetRingExtraForTest sets the room added to the streaming history ring beyond window + 2 blocks.
func SetRingExtraForTest(n int) { ringExtra = n }

func SetSeqAsmBackoffForTest(n int) { seqAsmBackoff = n }

// SetRingFloorForTest sets the minimum history ring size for windows of 256 KiB or more.
func SetRingFloorForTest(n int) { ringFloor = n }
