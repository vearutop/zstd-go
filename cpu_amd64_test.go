//go:build amd64 && !purego

package zstd

import "testing"

func TestCPUDetect(t *testing.T) {
	t.Logf("BMI1=%v BMI2=%v", hasBMI1, hasBMI2)
}
