package zstd

import "io"

func init() { strictHufDefault = true } // the existing differential tests expect libzstd's exact behavior

// SetStrictHufDefaultForTest switches the package-wide strict default (tests of the default fast path turn it off).
func SetStrictHufDefaultForTest(v bool) { strictHufDefault = v }

// SetRingExtraForTest sets the room added to the streaming history ring beyond window + 2 blocks.
func SetRingExtraForTest(n int) { ringExtra = n }

func SetSeqAsmBackoffForTest(n int) { seqAsmBackoff = n }

// SetRingFloorForTest sets the minimum history ring size for windows of 256 KiB or more.
func SetRingFloorForTest(n int) { ringFloor = n }

// NewStreamingReader is a synchronous Reader that never takes the single-pass shortcut for small frames, i.e. with the
// streaming semantics the pipeline always has.
func NewStreamingReader(r io.Reader, dec *Decoder) *Reader {
	z := NewReader(r, WithDecoder(dec))
	z.s.noShortcut = true
	return z
}

// Params of the Profile* writer options.
var (
	ProfileFastestParams         = profileFastest
	ProfileDefaultParams         = profileDefault
	ProfileBestCompressionParams = profileBestCompression
)

// Low-level API kept unexported for users, exposed to the external tests.
type (
	Encoder         = encoder
	StreamWriter    = streamWriter
	CDict           = preparedDict
	Directive       = directive
	FrameHeader     = pubFrameHeader
	DictContentType = dictContentType
)

const (
	Continue = eContinue
	Flush    = eFlush
	End      = eEnd

	DictAuto = dictAuto
	DictRaw  = dictRaw
	DictFull = dictFull

	ContentSizeUnknown = contentSizeUnknown
	ContentSizeError   = contentSizeError
)

var (
	NewEncoder              = newEncoder
	NewStreamWriter         = newStreamWriter
	DecompressBound         = decompressBound
	DecompressionMargin     = decompressionMargin
	DictIDFromDict          = dictIDFromDict
	DictIDFromFrame         = dictIDFromFrame
	FindDecompressedSize    = findDecompressedSize
	FindFrameCompressedSize = findFrameCompressedSize
	GetDecompressedSize     = getDecompressedSize
	GetFrameContentSize     = getFrameContentSize
	GetFrameHeader          = pubGetFrameHeader
	IsFrame                 = isFrame
	IsSkippableFrame        = isSkippableFrame
	ReadSkippableFrame      = readSkippableFrame
	LoadDictType            = loadDictType
	ErrPledgedSize          = errPledgedSize
	ContentSize             = contentSize
)

func (d *Dict) CDictLevel(level int) *CDict            { return d.cdictLevel(level) }
func (d *Dict) CDictAdvanced(p Params) (*CDict, error) { return d.cdictFromParams(p) }

var NewDStream = newDStream

type DStream = dStream

// DecodeAll is the (src, dst) spelling the tests use for both this package and klauspost's decoder.
func (d *Decoder) DecodeAll(src, dst []byte) ([]byte, error) { return d.Decode(dst, src) }
