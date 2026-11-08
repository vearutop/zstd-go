package zstd

// SetTrimSlackDivForTest lets the external benchmarks change the streaming history slack.
func SetTrimSlackDivForTest(d int) { trimSlackDiv = d }
