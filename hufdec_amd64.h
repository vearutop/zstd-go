// Body of the Huffman 4-stream fast loops, included by hufdec_amd64.s once per variant. The including file defines
// FN_X1, FN_X2 (function names) and the bit operations:
//   SHR_SH(src, dst)  dst = src >> BX (BX holds 64-tableLog; may clobber CX)
//   SHL_CX(r)         r <<= CX (count already in CX, below 64)
//   CTZ(src, dst)     dst = trailing zeros of src (src != 0)
//
// Registers: R8-R11 bit containers, R12-R15 input pointers (src+ip[s]), SI table, DX output pointer of the current
// stream, AX/CX scratch. The output pointers live at 0..24(SP). The streams are decoded one after the other within an
// iteration (the Go loop interleaves them); the caller guarantees that their outputs do not overlap, so the order does
// not change the result.

// RELOAD is the stream reload of the C loop: ip -= ctz>>3; bits = (load64(ip)|1) << (ctz&7).
#define RELOAD(B, I) \
	CTZ(B, AX); \
	MOVL AX, CX; \
	SHRL $3, AX; \
	SUBQ AX, I; \
	ANDL $7, CX; \
	MOVQ (I), B; \
	ORQ  $1, B; \
	SHL_CX(B)

// X1_SYM decodes one symbol: idx = bits >> (64-tl); bits <<= nb[idx]; out[j] = sym[idx].
#define X1_SYM(B, j)              \
	SHR_SH(B, AX); \
	MOVBLZX 4096(SI)(AX*1), CX; \
	SHL_CX(B); \
	MOVBLZX (SI)(AX*1), AX; \
	MOVB    AL, j(DX)

#define X1_STREAM(B, I, off) \
	MOVQ off(SP), DX; \
	X1_SYM(B, 0); \
	X1_SYM(B, 1); \
	X1_SYM(B, 2); \
	X1_SYM(B, 3); \
	X1_SYM(B, 4); \
	ADDQ $5, DX; \
	MOVQ DX, off(SP); \
	RELOAD(B, I)

// X2_SYM decodes one entry: e = x2[bits >> 53]; out[0:2] = e.seq; bits <<= e.nb; out += e.length.
#define X2_SYM(B)               \
	MOVQ B, AX; \
	SHRQ $53, AX; \
	MOVL (SI)(AX*4), CX; \
	MOVW CX, (DX); \
	SHRL $16, CX; \
	SHL_CX(B); \
	SHRL $8, CX; \
	ADDQ CX, DX

#define X2_STREAM(B, I, off) \
	MOVQ off(SP), DX; \
	X2_SYM(B); \
	X2_SYM(B); \
	X2_SYM(B); \
	X2_SYM(B); \
	X2_SYM(B); \
	MOVQ DX, off(SP); \
	RELOAD(B, I)

// LOAD_STATE loads the bit containers, the input pointers and the output pointers.
#define LOAD_STATE                        \
	MOVQ dst+8(FP), DX; \
	MOVQ op+40(FP), AX; \
	MOVQ 0(AX), CX; ADDQ DX, CX; MOVQ CX, 0(SP); \
	MOVQ 8(AX), CX; ADDQ DX, CX; MOVQ CX, 8(SP); \
	MOVQ 16(AX), CX; ADDQ DX, CX; MOVQ CX, 16(SP); \
	MOVQ 24(AX), CX; ADDQ DX, CX; MOVQ CX, 24(SP); \
	MOVQ src+16(FP), DX; \
	MOVQ ip+32(FP), AX; \
	MOVQ 0(AX), R12; ADDQ DX, R12; \
	MOVQ 8(AX), R13; ADDQ DX, R13; \
	MOVQ 16(AX), R14; ADDQ DX, R14; \
	MOVQ 24(AX), R15; ADDQ DX, R15; \
	MOVQ bits+24(FP), AX; \
	MOVQ 0(AX), R8; \
	MOVQ 8(AX), R9; \
	MOVQ 16(AX), R10; \
	MOVQ 24(AX), R11

// STORE_STATE stores them back as offsets.
#define STORE_STATE                       \
	MOVQ bits+24(FP), AX; \
	MOVQ R8, 0(AX); \
	MOVQ R9, 8(AX); \
	MOVQ R10, 16(AX); \
	MOVQ R11, 24(AX); \
	MOVQ src+16(FP), DX; \
	MOVQ ip+32(FP), AX; \
	SUBQ DX, R12; MOVQ R12, 0(AX); \
	SUBQ DX, R13; MOVQ R13, 8(AX); \
	SUBQ DX, R14; MOVQ R14, 16(AX); \
	SUBQ DX, R15; MOVQ R15, 24(AX); \
	MOVQ dst+8(FP), DX; \
	MOVQ op+40(FP), AX; \
	MOVQ 0(SP), CX; SUBQ DX, CX; MOVQ CX, 0(AX); \
	MOVQ 8(SP), CX; SUBQ DX, CX; MOVQ CX, 8(AX); \
	MOVQ 16(SP), CX; SUBQ DX, CX; MOVQ CX, 16(AX); \
	MOVQ 24(SP), CX; SUBQ DX, CX; MOVQ CX, 24(AX)

// func FN_X1(sym, dst, src *byte, bits *[4]uint64, ip, op *[4]int, iters int, sh uint64)
// Runs iters (> 0) iterations of the X1 loop; the nb table follows sym at +4096.
TEXT FN_X1(SB), NOSPLIT, $32-64
	LOAD_STATE
	MOVQ sym+0(FP), SI
	MOVQ iters+48(FP), DI
	MOVQ sh+56(FP), BX

x1loop:
	X1_STREAM(R8, R12, 0)
	X1_STREAM(R9, R13, 8)
	X1_STREAM(R10, R14, 16)
	X1_STREAM(R11, R15, 24)
	DECQ DI
	JNZ  x1loop

	STORE_STATE
	RET

// func FN_X2(x2 *x2Elt, dst, src *byte, bits *[4]uint64, ip, op *[4]int, olimit int)
// Runs X2 loop iterations until op[3] >= olimit (at least one).
TEXT FN_X2(SB), NOSPLIT, $32-56
	LOAD_STATE
	MOVQ x2+0(FP), SI
	MOVQ olimit+48(FP), DI
	ADDQ dst+8(FP), DI

x2loop:
	X2_STREAM(R8, R12, 0)
	X2_STREAM(R9, R13, 8)
	X2_STREAM(R10, R14, 16)
	X2_STREAM(R11, R15, 24)
	MOVQ 24(SP), DX
	CMPQ DX, DI
	JB   x2loop

	STORE_STATE
	RET

#undef RELOAD
#undef X1_SYM
#undef X1_STREAM
#undef X2_SYM
#undef X2_STREAM
#undef LOAD_STATE
#undef STORE_STATE
