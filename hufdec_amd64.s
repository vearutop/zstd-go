//go:build amd64 && !purego

#include "textflag.h"

// Baseline x86-64: shifts by CL, BSF (the containers are never zero, see hufdec_amd64.go).
#define FN_X1 ·huf4X1LoopBase
#define FN_X2 ·huf4X2LoopBase
#define SHR_SH(src, dst) MOVQ src, dst; MOVL BX, CX; SHRQ CL, dst
#define SHL_CX(r) SHLQ CL, r
#define CTZ(src, dst) BSFQ src, dst
#include "hufdec_amd64.h"
#undef FN_X1
#undef FN_X2
#undef SHR_SH
#undef SHL_CX
#undef CTZ

// BMI1+BMI2: SHRX/SHLX, TZCNT.
#define FN_X1 ·huf4X1LoopBMI
#define FN_X2 ·huf4X2LoopBMI
#define SHR_SH(src, dst) SHRXQ BX, src, dst
#define SHL_CX(r) SHLXQ CX, r, r
#define CTZ(src, dst) TZCNTQ src, dst
#include "hufdec_amd64.h"
