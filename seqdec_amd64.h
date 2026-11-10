// Body of seqDecBase/seqDecBMI2 (seqdec_amd64.s includes it once per variant, with its own GETBITS).
// It is the loop of decState.sequences (decoder.go) restricted to "simple" sequences that are not the block's last:
// the stream stays inside the buffer, the block stays within outLim, the literals take the 16-byte copy or (at least
// 16 of them) an append within the capacity, the offset (>= 32) points into this call's output and the match fits the
// 32-byte wildCopy. Each sequence is decoded into
// registers; when it is not simple the loop returns with c holding the state before it, otherwise it is committed to c.
//
// Registers: R15 c, R8 v, R9 br, R10 off, R11 buf, SI/DI/BP the ll/of/ml entries then the new states,
// R12 offBase, R13 mlen, R14 llen, AX n, BX offset, CX li, DX repeat mode.

	MOVQ c+0(FP), R15
	MOVQ seqCtx_v(R15), R8
	MOVQ seqCtx_br(R15), R9
	MOVQ seqCtx_off(R15), R10
	MOVQ seqCtx_buf(R15), R11

loop:
	CMPQ seqCtx_rem(R15), $0
	JLE  done

	// le, oe, me: tables + 12*state
	MOVQ seqCtx_sl(R15), AX
	LEAQ (AX)(AX*2), AX
	MOVQ seqCtx_llt(R15), SI
	LEAQ (SI)(AX*4), SI
	MOVQ seqCtx_so(R15), AX
	LEAQ (AX)(AX*2), AX
	MOVQ seqCtx_oft(R15), DI
	LEAQ (DI)(AX*4), DI
	MOVQ seqCtx_sm(R15), AX
	LEAQ (AX)(AX*2), AX
	MOVQ seqCtx_mlt(R15), BP
	LEAQ (BP)(AX*4), BP

	// refill (stop where the Go loop marks the stream bad)
	MOVQ R9, AX
	SHRQ $3, AX
	SUBQ AX, R10
	JLT  done
	ANDQ $7, R9
	MOVQ (R11)(R10*1), R8

	// offBase = oe.base + get(oe.xb) (uint32)
	MOVBQZX dEntry_xb(DI), DX
	GETBITS(DX, AX)
	ADDL    dEntry_base(DI), AX
	MOVQ    AX, R12

	// mlen = me.base + get(me.xb)
	MOVBQZX dEntry_xb(BP), DX
	GETBITS(DX, AX)
	MOVL    dEntry_base(BP), DX
	LEAQ    (AX)(DX*1), R13

	// refill
	MOVQ R9, AX
	SHRQ $3, AX
	SUBQ AX, R10
	JLT  done
	ANDQ $7, R9
	MOVQ (R11)(R10*1), R8

	// llen = le.base + get(le.xb)
	MOVBQZX dEntry_xb(SI), DX
	GETBITS(DX, AX)
	MOVL    dEntry_base(SI), DX
	LEAQ    (AX)(DX*1), R14

	// states, in the order sl, sm, so: ns + get(nb)
	MOVBQZX dEntry_nb(SI), DX
	GETBITS(DX, AX)
	MOVWQZX dEntry_ns(SI), DX
	LEAQ    (AX)(DX*1), SI
	MOVBQZX dEntry_nb(BP), DX
	GETBITS(DX, AX)
	MOVWQZX dEntry_ns(BP), DX
	LEAQ    (AX)(DX*1), BP
	MOVBQZX dEntry_nb(DI), DX
	GETBITS(DX, AX)
	MOVWQZX dEntry_ns(DI), DX
	LEAQ    (AX)(DX*1), DI

	// n-blockStart+llen+mlen <= outLim
	MOVQ seqCtx_n(R15), AX
	MOVQ AX, BX
	SUBQ seqCtx_blockStart(R15), BX
	ADDQ R14, BX
	ADDQ R13, BX
	CMPQ BX, seqCtx_outLim(R15)
	JGT  done

	// li+llen <= len(lits); then the 16-byte copy (llen <= 16, li+16 <= cap(lits), n+16 <= cap(out))
	// or an append that does not grow out (llen >= 16, n+llen <= cap(out))
	MOVQ seqCtx_li(R15), CX
	LEAQ (CX)(R14*1), DX
	CMPQ DX, seqCtx_litLen(R15)
	JGT  done
	CMPQ R14, $16
	JGT  litlong
	LEAQ 16(CX), DX
	CMPQ DX, seqCtx_litCap(R15)
	JGT  litlong
	LEAQ 16(AX), DX
	CMPQ DX, seqCtx_ocap(R15)
	JLE  litok

litlong:
	CMPQ R14, $16
	JLT  done
	LEAQ (AX)(R14*1), DX
	CMPQ DX, seqCtx_ocap(R15)
	JGT  done

litok:

	// offset (BX) and repeat update mode (DX): 0 none, 1 reps = (off, r0, r2), 2 reps = (off, r0, r1)
	CMPQ R12, $3
	JHI  newoff
	LEAQ -1(R12), DX
	TESTQ R14, R14
	JNZ  code
	INCQ DX

code:
	CMPQ DX, $1
	JEQ  rep1
	JHI  rep23
	MOVL seqCtx_reps+0(R15), BX
	JMP  check

rep1:
	MOVL seqCtx_reps+4(R15), BX
	JMP  check

rep23:
	CMPQ DX, $3
	JEQ  rep3
	MOVL seqCtx_reps+8(R15), BX
	JMP  check

rep3:
	MOVL seqCtx_reps+0(R15), BX
	DECL BX
	MOVL $2, DX
	JMP  check

newoff:
	MOVL R12, BX
	SUBL $3, BX
	MOVL $2, DX

check:
	// 32 <= off <= n+llen-base, n+llen+mlen+32 <= cap(out)
	CMPQ BX, $32
	JLT  done
	LEAQ (AX)(R14*1), R12
	SUBQ seqCtx_base(R15), R12
	CMPQ BX, R12
	JGT  done
	LEAQ 32(AX)(R14*1), R12
	ADDQ R13, R12
	CMPQ R12, seqCtx_ocap(R15)
	JGT  done

	// commit
	MOVQ R8, seqCtx_v(R15)
	MOVQ R9, seqCtx_br(R15)
	MOVQ R10, seqCtx_off(R15)
	MOVQ SI, seqCtx_sl(R15)
	MOVQ DI, seqCtx_so(R15)
	MOVQ BP, seqCtx_sm(R15)
	TESTQ DX, DX
	JZ   repsdone
	MOVL seqCtx_reps+0(R15), R12
	CMPQ DX, $1
	JEQ  reps1
	MOVL seqCtx_reps+4(R15), SI
	MOVL SI, seqCtx_reps+8(R15)

reps1:
	MOVL R12, seqCtx_reps+4(R15)
	MOVL BX, seqCtx_reps+0(R15)

repsdone:
	MOVQ seqCtx_out(R15), SI
	ADDQ AX, SI
	MOVQ seqCtx_lits(R15), DI
	ADDQ CX, DI
	CMPQ R14, $16
	JGT  cplong
	LEAQ 16(CX), DX
	CMPQ DX, seqCtx_litCap(R15)
	JGT  cplong
	LEAQ 16(AX), DX
	CMPQ DX, seqCtx_ocap(R15)
	JGT  cplong

	// 16 literal bytes (as two 8-byte moves, like the Go loop)
	MOVQ (DI), DX
	MOVQ DX, (SI)
	MOVQ 8(DI), DX
	MOVQ DX, 8(SI)
	JMP  cpdone

cplong:
	// exactly llen (>= 16) bytes: 16-byte steps, the last one ending at llen
	LEAQ -16(R14), DX
	XORQ R12, R12

cploop:
	CMPQ  R12, DX
	JGE   cptail
	MOVOU (DI)(R12*1), X0
	MOVOU X0, (SI)(R12*1)
	ADDQ  $16, R12
	JMP   cploop

cptail:
	MOVOU (DI)(DX*1), X0
	MOVOU X0, (SI)(DX*1)

cpdone:
	ADDQ R14, CX
	MOVQ CX, seqCtx_li(R15)
	ADDQ R14, SI
	ADDQ R14, AX

	// wildCopy: 32-byte steps from SI-off to SI; off >= 32, so each step reads only bytes before the ones it writes
	MOVQ SI, DI
	SUBQ BX, DI
	XORQ CX, CX

copy:
	CMPQ  CX, R13
	JGE   copied
	MOVOU (DI)(CX*1), X0
	MOVOU 16(DI)(CX*1), X1
	MOVOU X0, (SI)(CX*1)
	MOVOU X1, 16(SI)(CX*1)
	ADDQ  $32, CX
	JMP   copy

copied:
	ADDQ R13, AX
	MOVQ AX, seqCtx_n(R15)
	DECQ seqCtx_rem(R15)
	JMP  loop

done:
	RET
