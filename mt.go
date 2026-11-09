package zstd

import (
	"encoding/binary"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Multithreaded compression, a port of the job layout of zstdmt_compress.c for ZSTD_compress2 with nbWorkers > 0.
//
// The input is cut into jobs of targetSectionSize bytes (the last one is shorter). Job 0 is compressed like the start of an
// ordinary frame; every later job is compressed by a fresh encoder state that has the last targetPrefixSize bytes of the
// previous job loaded as a raw-content prefix (history for the match finder, nothing else), has no repeat offsets
// (ZSTD_invalidateRepCodes) and builds its own entropy tables, so the decoder's carried-over state can never be referenced.
// The jobs' blocks are concatenated behind job 0's frame header; only the last block of the last job is flagged; the frame
// checksum, if any, is the xxhash of the whole input. The bytes depend on JobSize and OverlapLog but not on NbWorkers, which
// is what lets the output be checked against libzstd for any worker count.

const (
	mtJobSizeMin = 512 << 10    // ZSTDMT_JOBSIZE_MIN
	mtJobSizeMax = 1 << 30      // ZSTDMT_JOBSIZE_MAX (64-bit)
	mtJobLogMax  = 30           // ZSTDMT_JOBLOG_MAX (64-bit)
	mtWorkersMax = 256          // ZSTDMT_NBWORKERS_MAX (64-bit)
	mtChunk      = 4 * blockMax // ZSTDMT_compressionJob compresses a job in chunks of four blocks
	mtHeaderSlop = 32           // room for a frame header and the checksum when sizing output
)

// mtOverlapLogDefault is ZSTDMT_overlapLog_default.
func mtOverlapLogDefault(s strategy) int {
	switch s {
	case stratBtUltra2:
		return 9
	case stratBtUltra, stratBtOpt:
		return 8
	case stratBtLazy2, stratLazy2:
		return 7
	}
	return 6
}

// mtGeometry returns targetSectionSize and targetPrefixSize (ZSTDMT_initCStream_internal, ZSTDMT_computeOverlapSize and
// ZSTDMT_computeTargetJobLog without long distance matching).
func mtGeometry(cp cParams, jobSize, overlapLog int) (section, prefix int) {
	ovl := overlapLog
	if ovl == 0 {
		ovl = mtOverlapLogDefault(cp.strategy)
	}
	if rlog := 9 - ovl; rlog < 8 {
		prefix = 1 << (int(cp.windowLog) - rlog)
	}
	section = jobSize
	if section == 0 {
		section = 1 << min(max(20, int(cp.windowLog)+2), mtJobLogMax)
	}
	return max(section, prefix), prefix
}

// compressMT is ZSTD_compress2 with nbWorkers > 0 for a source larger than ZSTDMT_JOBSIZE_MIN, r being the resolved request.
func (e *Encoder) compressMT(dst, src []byte, r *request) ([]byte, error) {
	if r.dict != nil && r.cd == nil {
		return dst, ErrUnsupported // a RefPrefix becomes a CDict in libzstd's multithreaded mode: not ported
	}
	n := len(src)
	section, prefixMax := mtGeometry(r.cp, r.jobSize, r.overlapLog)
	nJobs := (n + section - 1) / section
	workers := min(r.nbWorkers, nJobs)
	for len(e.mtEnc) < workers {
		e.mtEnc = append(e.mtEnc, NewEncoder(nil))
	}
	outs := make([][]byte, nJobs)
	var sum uint64
	var wg sync.WaitGroup
	if r.fp.checksum { // the checksum covers the whole input, in order (a single job writes it itself in libzstd)
		wg.Add(1)
		go func() { sum = xxhash.Sum64(src); wg.Done() }()
	}
	next := make(chan int, nJobs)
	for j := 0; j < nJobs; j++ {
		next <- j
	}
	close(next)
	for w := 0; w < workers; w++ {
		enc := e.mtEnc[w]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				lo := j * section
				hi := min(lo+section, n)
				var prefix []byte
				if j > 0 {
					prefix = src[lo-min(section, prefixMax) : lo] // the tail of the previous (full) job
				}
				outs[j] = enc.mtJob(r, src[lo:hi], prefix, uint64(n), j == 0, hi == n, nil)
			}
		}()
	}
	wg.Wait()
	total := mtHeaderSlop
	for _, o := range outs {
		total += len(o)
	}
	if cap(dst)-len(dst) < total {
		dst = append(make([]byte, 0, len(dst)+total), dst...)
	}
	for _, o := range outs {
		dst = append(dst, o...)
	}
	if r.fp.checksum {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(sum))
	}
	return dst, nil
}

// mtJob is ZSTDMT_compressionJob: the blocks of job (preceded by the frame header for the first job) compressed with prefix as
// history. total is the size of the whole frame content. out is a buffer to reuse for the result (nil: allocate).
func (e *Encoder) mtJob(r *request, job, prefix []byte, total uint64, first, last bool, out []byte) []byte {
	jr := *r
	if first {
		jr.alias = len(job) > 0 && jr.dict == nil // read the source in place
	} else {
		jr.pledged = uint64(len(job))
		jr.fp.checksum = false
		jr.cd, jr.dict, jr.alias = nil, nil, false
		if len(prefix) > 0 {
			jr.dict = &Dict{reps: [3]uint32{1, 4, 8}, raw: prefix, content: prefix}
		}
	}
	L := e.begin(job, &jr)
	if need := len(job) + len(job)>>3 + mtHeaderSlop; cap(out) < need {
		out = make([]byte, 0, need)
	}
	out = out[:0]
	var produced, consumed int64
	hdrLen := 0
	if first {
		out = e.header(out, total)
		hdrLen = len(out)
	} else {
		// the header is flushed (and later overwritten) before the first chunk and counts as produced bytes there;
		// forceMaxWindow: the prefix is subject to the window like any other data; invalid repeat offsets
		produced = int64(len(e.header(nil, jr.pledged)))
		// the prefix directly precedes the source in memory, so libzstd's window stays contiguous: the plain (noDict) match
		// finders run, with the prefix as ordinary history, not as an extDict segment
		e.dictLimit, e.copyMode = e.lowLimit, false
		e.loadedDictEnd = 0
		e.reps = [3]uint32{}
	}
	if len(job) == 0 && last { // a frame must end with a last block
		out = blockHeader(out, true, 0, 0)
	}
	for off := 0; off < len(job); off += mtChunk {
		end := min(off+mtChunk, len(job))
		before := len(out)
		out = e.chunk(out, job[off:end], L, off, last && end == len(job), consumed-produced)
		consumed += int64(end - off)
		produced += int64(len(out) - before)
		if first && off == 0 {
			produced += int64(hdrLen) // the header is written by the first call
		}
	}
	if e.aliased { // do not keep the caller's slice alive
		e.buf, e.aliased = nil, false
	}
	return out
}
