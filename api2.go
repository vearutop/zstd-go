package zstd

// ZSTD_compress2-style API of the Encoder: parameters (ZSTD_CCtx_setParameter), dictionaries
// (ZSTD_CCtx_loadDictionary*, ZSTD_CCtx_refPrefix*, ZSTD_CCtx_refCDict) and single-shot compression
// (ZSTD_compress2: the pledged size is the source size).

// CDict is a dictionary prepared for compression (ZSTD_CDict): the dictionary and its match tables for one set of
// parameters.
type CDict struct {
	d  *Dict
	cd *cdict
}

// CDictLevel is ZSTD_createCDict(dict, level). With ZSTD_CCtx_refCDict the CDict's level replaces the requested level.
func (d *Dict) CDictLevel(level int) *CDict {
	if level == 0 {
		level = defaultLevel
	}
	return &CDict{d, d.cdict(level)}
}

// CDictAdvanced is ZSTD_createCDict_advanced2: the tables are built for the (explicit) parameters p; the CDict has no
// compression level of its own (ZSTD_NO_CLEVEL), so compressions with it always use its parameters when the
// source is small, regardless of the source size cutoff of the level.
func (d *Dict) CDictAdvanced(p Params) (*CDict, error) {
	c, err := p.resolved()
	if err != nil {
		return nil, err
	}
	if c.DedicatedDictSearch || c.TargetCBlockSize != 0 {
		return nil, ErrUnsupported
	}
	return &CDict{d, d.cdictAdvanced(&c)}, nil
}

// SetParams replaces the compression parameters used by Compress2 (all of ZSTD_CCtx_setParameter at once).
func (e *Encoder) SetParams(p Params) error {
	if _, err := p.resolved(); err != nil {
		return err
	}
	e.params = p
	return nil
}

// RefCDict is ZSTD_CCtx_refCDict: it replaces any dictionary set before; c == nil removes the dictionary.
func (e *Encoder) RefCDict(c *CDict) {
	e.clearDicts()
	e.refCD = c
}

// LoadDictionary is ZSTD_CCtx_loadDictionary_advanced: the dictionary is digested on the first Compress2 with the
// parameters of that moment and kept for the following ones. It replaces any dictionary set before.
func (e *Encoder) LoadDictionary(b []byte, typ DictContentType) error {
	e.clearDicts()
	if len(b) == 0 {
		return nil
	}
	d, err := LoadDictType(b, typ)
	if err != nil {
		return err
	}
	e.local = d
	return nil
}

// RefPrefix is ZSTD_CCtx_refPrefix_advanced: b is used as a dictionary for the next frame only, without a CDict (use
// DictRaw for ZSTD_CCtx_refPrefix).
func (e *Encoder) RefPrefix(b []byte, typ DictContentType) error {
	e.clearDicts()
	if len(b) == 0 {
		return nil
	}
	d, err := LoadDictType(b, typ)
	if err != nil {
		return err
	}
	e.prefix = d
	return nil
}

func (e *Encoder) clearDicts() { e.refCD, e.local, e.localCD, e.prefix = nil, nil, nil, nil }

// Compress2 appends a frame of src to dst like ZSTD_compress2 with the parameters of SetParams and the dictionary set
// with RefCDict / LoadDictionary / RefPrefix. Without any of them a dictionary given to NewEncoder is used as
// ZSTD_createCDict(dict, e.Level) referenced by ZSTD_CCtx_refCDict.
func (e *Encoder) Compress2(dst, src []byte) ([]byte, error) {
	r, err := e.buildRequest(uint64(len(src)))
	if err != nil {
		return dst, err
	}
	return e.frame(src, dst, e.begin(src, r)), nil
}

// buildRequest is ZSTD_CCtx_init_compressStream2: the requested parameters and dictionary resolved for a frame of
// pledged bytes (contentSizeUnknown if not known).
func (e *Encoder) buildRequest(pledged uint64) (*request, error) {
	c, err := e.params.resolved()
	if err != nil {
		return nil, err
	}
	if c.DedicatedDictSearch || c.TargetCBlockSize != 0 {
		return nil, ErrUnsupported
	}
	r := &request{pledged: pledged}
	c.fill(r)
	if pledged == contentSizeUnknown {
		r.fp.contentSize = false // ZSTD_resetCCtx_internal
	}
	var dictSize uint64
	mode := cpmNoAttachDict
	switch {
	case e.prefix != nil:
		r.dict, dictSize = e.prefix, uint64(len(e.prefix.raw))
		e.prefix = nil // single use
	default:
		cdc := e.refCD
		if cdc == nil && e.local != nil {
			if e.localCD == nil { // ZSTD_initLocalDict: built once, with the parameters of the first use
				e.localCD = &CDict{e.local, e.local.cdictAdvanced(&c)}
			}
			cdc = e.localCD
		} else if cdc == nil && e.dict != nil && len(e.dict.raw) > 0 {
			cdc = e.dict.CDictLevel(e.level())
		}
		if cdc != nil {
			r.dict, r.cd, dictSize = cdc.d, cdc.cd, cdc.cd.dictSize
			if e.refCD != nil || e.local == nil { // an explicit CDict's level takes priority over the requested one
				c.level = cdc.cd.level
			}
			if shouldAttachDict(cdc.cd, c.ForceAttachDict, pledged) {
				mode = cpmAttachDict
			}
		}
	}
	r.cp = getCParamsFromCCtxParams(&c, pledged, dictSize, mode)
	r.postSplit = resolveBlockSplitter(c.PostBlockSplitter, r.cp)
	r.useRow = resolveRow(c.UseRowMatchFinder, r.cp)
	if r.cp.strategy >= stratBtOpt && r.cp.windowLog >= 27 {
		return nil, ErrUnsupported // ZSTD_resolveEnableLdm: long distance matching is not ported
	}
	return r, nil
}
