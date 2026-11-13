//go:build amd64 && !purego

#include "textflag.h"

// func prefetchRow(hashTable *uint32, tagTable *uint8, relRow, rowLog uint32)
TEXT ·prefetchRow(SB), NOSPLIT, $0-24
	MOVQ hashTable+0(FP), SI
	MOVQ tagTable+8(FP), DI
	MOVL relRow+16(FP), AX
	MOVL rowLog+20(FP), CX
	LEAQ (SI)(AX*4), SI
	ADDQ AX, DI
	PREFETCHT0 (SI)
	CMPL CX, $5
	JLT  tag
	PREFETCHT0 64(SI)
tag:
	PREFETCHT0 (DI)
	CMPL CX, $6
	JNE  done
	PREFETCHT0 32(DI)
done:
	RET
