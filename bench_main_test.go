package zstd_test

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"testing"
)

// libzstdVersion is set by bench_ref_hook_test.go with -tags libzstd.
var libzstdVersion string

// TestMain prints the versions of the compared libraries as benchstat configuration lines ("key: value") when benchmarks run.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("test.bench"); f != nil && f.Value.String() != "" {
		ver := "unknown"
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, d := range bi.Deps {
				if d.Path == "github.com/klauspost/compress" {
					ver = d.Version
				}
			}
		}
		fmt.Println("klauspost-compress:", ver)
		if libzstdVersion != "" {
			fmt.Println("libzstd:", libzstdVersion)
		}
	}
	os.Exit(m.Run())
}
