package zstd

import (
	"errors"
	"fmt"
)

// Switch is ZSTD_ParamSwitch_e (ZSTD_ps_auto, ZSTD_ps_enable, ZSTD_ps_disable).
type Switch uint8

const (
	SwitchAuto Switch = iota
	SwitchEnable
	SwitchDisable
)

// AttachPref is ZSTD_dictAttachPref_e (ZSTD_c_forceAttachDict).
type AttachPref uint8

const (
	AttachDefault AttachPref = iota // ZSTD_dictDefaultAttach
	AttachForce                     // ZSTD_dictForceAttach
	CopyForce                       // ZSTD_dictForceCopy
	LoadForce                       // ZSTD_dictForceLoad
)

// DictContentType is ZSTD_dictContentType_e.
type DictContentType uint8

const (
	DictAuto DictContentType = iota // ZSTD_dct_auto: zstd-format when the magic matches, raw content otherwise
	DictRaw                         // ZSTD_dct_rawContent
	DictFull                        // ZSTD_dct_fullDict: must be zstd-format
)

// Params mirrors the ZSTD_c_* parameters of ZSTD_CCtx_setParameter that Encoder.Compress2 honours; the zero value is
// libzstd's default configuration (level 3, no checksum, content size and dictionary id written).
// Fields of 0 mean "derive from the level", like ZSTD_CCtx_setParameter(…, 0).
type Params struct {
	Level                                                                     int // ZSTD_c_compressionLevel; 0 is the default level 3
	WindowLog, HashLog, ChainLog, SearchLog, MinMatch, TargetLength, Strategy int // explicit compression parameters (Strategy: 1 fast .. 9 btultra2)

	NoContentSize bool // ZSTD_c_contentSizeFlag = 0
	Checksum      bool // ZSTD_c_checksumFlag
	NoDictID      bool // ZSTD_c_dictIDFlag = 0
	Magicless     bool // ZSTD_c_format = ZSTD_f_zstd1_magicless

	SrcSizeHint int // ZSTD_c_srcSizeHint

	LiteralCompression  Switch // ZSTD_c_literalCompressionMode: enable = Huffman, disable = raw literals
	UseRowMatchFinder   Switch // ZSTD_c_useRowMatchFinder
	ForceAttachDict     AttachPref
	DedicatedDictSearch bool // ZSTD_c_enableDedicatedDictSearch

	PostBlockSplitter  Switch // ZSTD_c_splitAfterSequences
	PreBlockSplitLevel int    // ZSTD_c_blockSplitterLevel (0 auto, 1 only full blocks, 2-6)
	MaxBlockSize       int    // ZSTD_c_maxBlockSize (0: 128 KiB)
	TargetCBlockSize   int    // ZSTD_c_targetCBlockSize

	// NbWorkers > 0 is ZSTD_c_nbWorkers: Compress2 cuts the input into jobs that are compressed in parallel and joined into
	// one frame, byte-identical to libzstd with the same nbWorkers parameters (the output depends on JobSize and OverlapLog, not
	// on NbWorkers). Inputs of 512 KiB or less are compressed as with 0. Each job after the first sees only the last OverlapLog
	// share of the window of the previous job as history, and starts with fresh entropy tables and no repeat offsets, so it
	// compresses a little worse than one thread. The streaming API (CompressStream2, StreamWriter) makes the same frame with at
	// most NbWorkers jobs in memory (unlike the single-threaded stream, which keeps the whole frame), for any length. Not
	// supported with a raw RefPrefix or LDM.
	NbWorkers int
	// JobSize is ZSTD_c_jobSize: the bytes of input per job (0: 1 << max(20, windowLog + 2), at most 1 GiB; at least 512 KiB).
	JobSize int
	// OverlapLog is ZSTD_c_overlapLog: how much of the previous job a job reloads, as 9 - OverlapLog shifts of the window size
	// (0: by strategy, 6 for fast to lazy = window/8, 9 for btultra2 = the whole window; 1 = none).
	OverlapLog int
}

// ctxParams is the resolved subset of ZSTD_CCtx_params the encoder works with.
type ctxParams struct {
	Params
	level int // 0 resolved to the default
}

// ErrUnsupported is returned for parameter combinations the port does not implement.
var ErrUnsupported = errors.New("zstd: unsupported parameter combination")

func bound(name string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("zstd: parameter %s out of bounds [%d, %d]: %d", name, lo, hi, v)
	}
	return nil
}

// validate is the BOUNDCHECKs of ZSTD_CCtxParams_setParameter.
func (p *Params) validate() error {
	for _, c := range []struct {
		n       string
		v       int
		lo, hi  int
		zeroOK  bool
		clampLo int
	}{
		{"windowLog", p.WindowLog, windowLogAbsMin, windowLogMax, true, 0},
		{"hashLog", p.HashLog, 6, 30, true, 0},
		{"chainLog", p.ChainLog, 6, 30, true, 0},
		{"searchLog", p.SearchLog, 1, 30, true, 0},
		{"minMatch", p.MinMatch, 3, 7, true, 0},
		{"targetLength", p.TargetLength, 0, 1 << 17, true, 0},
		{"strategy", p.Strategy, 1, 9, true, 0},
		{"srcSizeHint", p.SrcSizeHint, 0, 1<<31 - 1, true, 0},
		{"preBlockSplitLevel", p.PreBlockSplitLevel, 0, 6, true, 0},
		{"maxBlockSize", p.MaxBlockSize, 1 << 10, 128 << 10, true, 0},
		{"literalCompression", int(p.LiteralCompression), 0, 2, true, 0},
		{"useRowMatchFinder", int(p.UseRowMatchFinder), 0, 2, true, 0},
		{"postBlockSplitter", int(p.PostBlockSplitter), 0, 2, true, 0},
		{"forceAttachDict", int(p.ForceAttachDict), 0, 3, true, 0},
	} {
		if c.zeroOK && c.v == 0 {
			continue
		}
		if err := bound(c.n, c.v, c.lo, c.hi); err != nil {
			return err
		}
	}
	if p.TargetCBlockSize != 0 && p.TargetCBlockSize < 1340 {
		// ZSTD_c_targetCBlockSize: raised to the minimum
	}
	if p.TargetCBlockSize > 128<<10 {
		return bound("targetCBlockSize", p.TargetCBlockSize, 1340, 128<<10)
	}
	return nil
}

func (p *Params) resolved() (ctxParams, error) {
	if err := p.validate(); err != nil {
		return ctxParams{}, err
	}
	c := ctxParams{Params: *p, level: p.Level}
	if c.level == 0 {
		c.level = defaultLevel
	}
	c.level = max(min(c.level, 22), -(1 << 17)) // ZSTD_cParam_clampBounds
	if c.TargetCBlockSize != 0 {
		c.TargetCBlockSize = max(c.TargetCBlockSize, 1340)
	}
	// ZSTD_cParam_clampBounds: the multithreading parameters are clamped, not rejected
	c.NbWorkers = max(0, min(c.NbWorkers, mtWorkersMax))
	if c.JobSize != 0 {
		c.JobSize = max(mtJobSizeMin, min(c.JobSize, mtJobSizeMax))
	}
	c.OverlapLog = max(0, min(c.OverlapLog, 9))
	return c, nil
}

func (c *ctxParams) overrides() cParams {
	return cParams{uint32(c.WindowLog), uint32(c.ChainLog), uint32(c.HashLog), uint32(c.SearchLog), uint32(c.MinMatch), uint32(c.TargetLength), strategy(c.Strategy)}
}

// getCParamsFromCCtxParams is ZSTD_getCParamsFromCCtxParams (LDM never enabled explicitly).
func getCParamsFromCCtxParams(c *ctxParams, srcSizeHint, dictSize uint64, mode cpMode) cParams {
	if srcSizeHint == contentSizeUnknown && c.SrcSizeHint > 0 {
		srcSizeHint = uint64(c.SrcSizeHint)
	}
	cp := getCParams(c.level, srcSizeHint, dictSize, mode)
	o := c.overrides()
	if o.windowLog != 0 {
		cp.windowLog = o.windowLog
	}
	if o.hashLog != 0 {
		cp.hashLog = o.hashLog
	}
	if o.chainLog != 0 {
		cp.chainLog = o.chainLog
	}
	if o.searchLog != 0 {
		cp.searchLog = o.searchLog
	}
	if o.minMatch != 0 {
		cp.minMatch = o.minMatch
	}
	if o.targetLength != 0 {
		cp.targetLength = o.targetLength
	}
	if o.strategy != 0 {
		cp.strategy = o.strategy
	}
	return adjustCParams(cp, srcSizeHint, dictSize, mode, psMode(c.UseRowMatchFinder))
}

// resolveRow is ZSTD_resolveRowMatchFinderMode.
func resolveRow(mode Switch, cp cParams) psMode {
	if mode != SwitchAuto {
		return psMode(mode)
	}
	if rowMatchFinderSupported(cp.strategy) && cp.windowLog > 14 {
		return psEnable
	}
	return psDisable
}

// resolveBlockSplitter is ZSTD_resolveBlockSplitterMode.
func resolveBlockSplitter(mode Switch, cp cParams) psMode {
	if mode != SwitchAuto {
		return psMode(mode)
	}
	if cp.strategy >= stratBtOpt && cp.windowLog >= 17 {
		return psEnable
	}
	return psDisable
}

// literalsDisabled is ZSTD_literalsCompressionIsDisabled.
func literalsDisabled(mode Switch, cp cParams) bool {
	switch mode {
	case SwitchEnable:
		return false
	case SwitchDisable:
		return true
	}
	return cp.strategy == stratFast && cp.targetLength > 0
}

// frameParams is ZSTD_frameParameters plus the format.
type frameParams struct {
	contentSize, checksum, noDictID, magicless bool
}

// request is what ZSTD_compressBegin_internal receives: resolved parameters, the pledged size and the dictionary.
type request struct {
	alias     bool    // the source may be read in place instead of being copied to the Encoder's buffer (EncodeAll)
	cp        cParams // params->cParams, before the adjustments for an attached or copied CDict
	useRow    psMode  // resolved
	postSplit psMode  // resolved
	fp        frameParams
	literal   Switch
	attach    AttachPref
	preSplit  int
	maxBlock  int    // resolved (never 0)
	pledged   uint64 // contentSizeUnknown if unknown
	dict      *Dict  // parsed dictionary (content, entropy tables), nil if none
	cd        *cdict // the CDict, nil when the dictionary is loaded directly (prefix)
	targetCB  int

	nbWorkers, jobSize, overlapLog int // multithreading (ZSTD_c_nbWorkers, jobSize, overlapLog), clamped
}

// frame params and common fields of a request from the user parameters.
func (c *ctxParams) fill(r *request) {
	r.fp = frameParams{contentSize: !c.NoContentSize, checksum: c.Checksum, noDictID: c.NoDictID, magicless: c.Magicless}
	r.literal, r.attach, r.preSplit, r.targetCB = c.LiteralCompression, c.ForceAttachDict, c.PreBlockSplitLevel, c.TargetCBlockSize
	r.maxBlock = c.MaxBlockSize
	if r.maxBlock == 0 {
		r.maxBlock = blockMax
	}
	r.nbWorkers, r.jobSize, r.overlapLog = c.NbWorkers, c.JobSize, c.OverlapLog
}

// rowUsed is ZSTD_rowMatchFinderUsed.
func rowUsed(s strategy, mode psMode) bool { return rowMatchFinderSupported(s) && mode == psEnable }
