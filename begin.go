package zstd

import (
	"encoding/binary"

	"github.com/cespare/xxhash/v2"
)

// NewEncoder returns an encoder; d may be nil.
func NewEncoder(d *Dict) *Encoder { return &Encoder{dict: d} }

// EncodeAll appends a complete frame of src to dst. Frame has content size (single segment) and checksum.
// It is ZSTD_compress_usingCDict (ZSTD_compressCCtx without a dictionary) at Encoder.Level; see Compress2 for the
// ZSTD_compress2 configuration.
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	return e.frame(src, dst, e.begin(src, e.legacyRequest(src)))
}

// legacyRequest is ZSTD_compressBegin_usingCDict_internal (ZSTD_compress_usingCDict), or ZSTD_compressCCtx without
// a dictionary.
func (e *Encoder) legacyRequest(src []byte) *request {
	n := uint64(len(src))
	r := &request{pledged: n, maxBlock: blockMax, fp: frameParams{contentSize: true, checksum: !e.NoChecksum}}
	level := e.level()
	if d := e.dict; d != nil && len(d.raw) > 0 {
		r.dict = d
		r.cd = d.cdict(level)
		cdp := r.cd.cp
		// the prepared dictionary is reused for small sources only; for large ones libzstd loads the dictionary content
		// into the context with the parameters of the source size.
		reuse := n < 128<<10 || n < r.cd.dictSize*6
		r.cp = cdp
		if !reuse {
			r.cp = getCParams(level, n, r.cd.dictSize, cpmUnknown)
		}
		// Increase window log to fit the entire dictionary and source: ZSTD_compressBegin_usingCDict_internal
		limited := min(n, 1<<19)
		limitedLog := uint32(1)
		if limited > 1 {
			limitedLog = highbit32(uint32(limited-1)) + 1
		}
		// ZSTD_CCtxParams_init_internal resolves the modes from the parameters before the window log is raised
		r.useRow = resolveRow(SwitchAuto, r.cp)
		r.postSplit = resolveBlockSplitter(SwitchAuto, r.cp)
		r.cp.windowLog = max(r.cp.windowLog, limitedLog)
		return r
	}
	r.cp = getCParams(level, n, 0, cpmUnknown)
	r.useRow = resolveRow(SwitchAuto, r.cp)
	r.postSplit = resolveBlockSplitter(SwitchAuto, r.cp)
	return r
}

func (e *Encoder) level() int {
	if e.Level == 0 {
		return defaultLevel
	}
	return e.Level
}

var attachDictSizeCutoffs = [...]uint64{8 << 10, 8 << 10, 16 << 10, 32 << 10, 32 << 10, 32 << 10, 32 << 10, 32 << 10, 8 << 10, 8 << 10}

// shouldAttachDict is ZSTD_shouldAttachDict.
func shouldAttachDict(cd *cdict, pref AttachPref, pledged uint64) bool {
	cutoff := attachDictSizeCutoffs[cd.cp.strategy]
	return cd.dds || ((pledged <= cutoff || pledged == contentSizeUnknown || pref == AttachForce) && pref != CopyForce)
}

// frame is ZSTD_compressEnd_public after begin: header, the blocks of src, epilogue. L is the dictionary length.
func (e *Encoder) frame(src, dst []byte, L int) []byte {
	pledged := uint64(len(src))
	dst = e.header(dst, pledged)
	windowSize := max(1, min(uint64(1)<<e.cp.windowLog, pledged))
	bsMax := int(min(uint64(e.blockMax), windowSize)) // cctx->blockSizeMax
	var savings int64
	for base := 0; ; {
		bs := optimalBlockSize(src[base:], bsMax, savings, e.preSplit, e.cp.strategy)
		end := base + bs
		before := len(dst)
		dst = e.block(dst, src[base:end], L, base, end == len(src))
		savings += int64(bs) - int64(len(dst)-before)
		base = end
		if end == len(src) {
			break
		}
	}
	if e.fp.checksum {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(xxhash.Sum64(src)))
	}
	return dst
}

// begin loads dictionary and source into e.buf, derives the working parameters (ZSTD_compressBegin_internal:
// ZSTD_resetCCtx_usingCDict by attaching or copying, or the full dictionary load) and sets up the tables. It returns
// the dictionary length in e.buf.
func (e *Encoder) begin(src []byte, r *request) (L int) {
	n := r.pledged
	e.copyMode, e.loadedDictEnd, e.dictLimit, e.lowLimit = false, 0, windowStartIndex, windowStartIndex
	e.nextToUpdate, e.hashCache, e.lazySkipping = windowStartIndex, [rowHashCacheSize]uint32{}, false
	e.fp, e.blockMax, e.preSplit, e.litOff = r.fp, r.maxBlock, r.preSplit, false
	e.postSplit = r.postSplit == psEnable
	if e.opt != nil {
		e.opt.noLit = r.literal == SwitchDisable
	}
	e.cd, e.dms = nil, nil
	e.idxShift = 0
	d := r.dict
	if d != nil && d.loadable() {
		L = len(d.content)
		if e.bufDict != d {
			e.buf = append(e.buf[:0], d.content...)
			e.bufDict = d
		}
		e.buf = append(e.buf[:L], src...)
	} else {
		e.buf = append(e.buf[:0], src...)
		e.bufDict = nil
	}
	e.dictID = 0
	if d != nil && len(d.raw) >= 8 {
		e.dictID = d.id
	}
	cd := r.cd
	switch {
	case cd != nil && cd.dictSize > 0 && r.attach != LoadForce &&
		(n < 128<<10 || n < cd.dictSize*6 || n == contentSizeUnknown || cd.level == 0):
		// ZSTD_resetCCtx_usingCDict
		e.cd = cd
		e.loadedDictEnd = uint32(L + windowStartIndex)
		e.dictLimit = e.loadedDictEnd
		if shouldAttachDict(cd, r.attach, n) {
			e.useRow = cd.row // the CDict overrides
			e.cp = adjustCParams(cd.cp, n, cd.dictSize, cpmAttachDict, r.useRow)
			e.cp.windowLog = r.cp.windowLog
			e.lowLimit = e.dictLimit // the attached dictionary lives in the CDict: the window starts at the source
			if L > 0 {
				e.dms = cd
			} else {
				e.loadedDictEnd = 0
			}
			e.growTables()
		} else {
			e.copyMode = L > 0
			e.useRow = cd.row
			e.cp = cd.cp
			e.cp.windowLog = r.cp.windowLog
			e.copyCDictTables()
			if L == 0 {
				e.loadedDictEnd, e.dictLimit = 0, windowStartIndex
			}
		}
	case L > 0:
		// dictionary content loaded into the context (ZSTD_compress_insertDictionary, dtlm fast, tfp forCCtx)
		e.loadedDictEnd = uint32(L + windowStartIndex)
		e.dictLimit = e.loadedDictEnd
		e.copyMode = true
		e.cp = r.cp
		e.useRow = rowUsed(e.cp.strategy, r.useRow)
		e.growTables()
		e.fillDictTables(d.content)
	default:
		e.cp = r.cp
		e.useRow = rowUsed(e.cp.strategy, r.useRow)
		e.growTables()
	}
	e.litOff = literalsDisabled(r.literal, e.cp)
	if e.cp.strategy >= stratBtOpt { // ZSTD_reset_matchState: optimal parser space, hashTable3
		if e.opt == nil {
			e.opt = new(optState)
		}
		e.opt.noLit = r.literal == SwitchDisable
		e.opt.litLengthSum = 0 // ZSTD_invalidateMatchState: force reset of btopt stats
		e.hashLog3 = 0
		if e.cp.minMatch == 3 {
			e.hashLog3 = min(hashLog3Max, e.cp.windowLog)
			e.h3 = grow(e.h3, 1<<e.hashLog3)
		}
	}
	e.reps = [3]uint32{1, 4, 8}
	e.prevHuf, e.prevFse = hufState{}, fseTables{}
	if d != nil && len(d.raw) >= 8 {
		e.reps = d.reps
		e.prevHuf = hufState{d.huffC, d.huffRepeat}
		e.prevFse = d.fse
	}
	return L
}
