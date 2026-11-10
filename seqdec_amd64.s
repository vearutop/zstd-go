//go:build amd64 && !purego

#include "textflag.h"
#include "go_asm.h"

// GETBITS(k, dst): dst = the next k bits (k <= 32) of the window R8 after R9 consumed bits, R9 += k; clobbers CX.
// It is rbits.get: v << br >> 1 >> (63 - k).

#define GETBITS(k, dst) \
	MOVQ R9, CX; \
	MOVQ R8, dst; \
	SHLQ CL, dst; \
	SHRQ $1, dst; \
	MOVL $63, CX; \
	SUBL k, CX; \
	SHRQ CL, dst; \
	ADDQ k, R9

// func seqDecBase(c *seqCtx)
TEXT ·seqDecBase(SB), NOSPLIT, $8-8
#include "seqdec_amd64.h"

#undef GETBITS
#define GETBITS(k, dst) \
	SHLXQ R9, R8, dst; \
	SHRQ $1, dst; \
	MOVL $63, CX; \
	SUBL k, CX; \
	SHRXQ CX, dst, dst; \
	ADDQ k, R9

// func seqDecBMI2(c *seqCtx)
TEXT ·seqDecBMI2(SB), NOSPLIT, $8-8
#include "seqdec_amd64.h"
