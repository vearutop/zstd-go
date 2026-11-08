package zstd

// SetTrimSlackDivForTest lets the external benchmarks change the streaming history slack.
func SetTrimSlackDivForTest(d int) { trimSlackDiv = d }

// SetTrimFloorForTest sets the minimum history size before shifting.
func SetTrimFloorForTest(n int) { trimFloor = n }
