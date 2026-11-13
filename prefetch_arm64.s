//go:build arm64 && !purego

#include "textflag.h"

// func prefetchRow(hashTable *uint32, tagTable *uint8, relRow, rowLog uint32)
TEXT ·prefetchRow(SB), NOSPLIT, $0-24
	MOVD  hashTable+0(FP), R0
	MOVD  tagTable+8(FP), R1
	MOVWU relRow+16(FP), R2
	MOVWU rowLog+20(FP), R3
	ADD   R2<<2, R0, R0
	ADD   R2, R1, R1
	PRFM  (R0), PLDL1KEEP
	CMPW  $5, R3
	BLT   tag
	PRFM  64(R0), PLDL1KEEP
tag:
	PRFM  (R1), PLDL1KEEP
	CMPW  $6, R3
	BNE   done
	PRFM  32(R1), PLDL1KEEP
done:
	RET
