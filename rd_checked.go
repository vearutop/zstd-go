//go:build !((amd64 || arm64) && !purego && !zstdcheck)

package zstd

import "encoding/binary"

func rd64(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i : i+8]) }
func rd32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i : i+4]) }

func p8(s []uint8, i uint32) *uint8                  { return &s[i] }
func p32(s []uint32, i uint32) *uint32               { return &s[i] }
func slice8(s []uint8, off uint32, n int) []uint8    { return s[off : off+uint32(n)] }
func slice32(s []uint32, off uint32, n int) []uint32 { return s[off : off+uint32(n)] }
