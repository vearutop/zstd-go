//go:build amd64 && !purego

package kseq

// Copied from github.com/klauspost/compress/internal/cpuinfo (BSD-3-Clause, see LICENSE).

//go:noescape
func x86extensions() (bmi1, bmi2 bool)

var hasBMI2 = func() bool { _, b := x86extensions(); return b }()
