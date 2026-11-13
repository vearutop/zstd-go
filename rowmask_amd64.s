//go:build amd64 && !purego

#include "textflag.h"

// func tagMatches(tagRow []uint8, tag uint8, n uint32) uint64
TEXT ·tagMatches(SB), NOSPLIT, $0-40
	MOVQ    tagRow_base+0(FP), SI
	MOVBLZX tag+24(FP), AX
	MOVL    n+28(FP), CX
	MOVL    AX, X0
	PUNPCKLBW X0, X0
	PUNPCKLWL X0, X0
	PSHUFL  $0, X0, X0
	MOVOU   (SI), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, AX
	CMPL    CX, $16
	JEQ     done
	MOVOU   16(SI), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, DX
	SHLQ    $16, DX
	ORQ     DX, AX
	CMPL    CX, $32
	JEQ     done
	MOVOU   32(SI), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, DX
	SHLQ    $32, DX
	ORQ     DX, AX
	MOVOU   48(SI), X1
	PCMPEQB X0, X1
	PMOVMSKB X1, DX
	SHLQ    $48, DX
	ORQ     DX, AX
done:
	MOVQ    AX, ret+32(FP)
	RET
