package zstd

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrorCode mirrors libzstd's ZSTD_ErrorCode for decoding errors.
type ErrorCode int

// Decoding error codes (values as in libzstd's ZSTD_ErrorCode).
const (
	ErrGeneric                      ErrorCode = 1
	ErrPrefixUnknown                ErrorCode = 10
	ErrVersionUnsupported           ErrorCode = 12
	ErrFrameParameterUnsupported    ErrorCode = 14
	ErrFrameParameterWindowTooLarge ErrorCode = 16
	ErrCorruptionDetected           ErrorCode = 20
	ErrChecksumWrong                ErrorCode = 22
	ErrLiteralsHeaderWrong          ErrorCode = 24
	ErrDictionaryCorrupted          ErrorCode = 30
	ErrDictionaryWrong              ErrorCode = 32
	ErrParameterUnsupported         ErrorCode = 40
	ErrParameterOutOfBound          ErrorCode = 42
	ErrTableLogTooLarge             ErrorCode = 44
	ErrMaxSymbolValueTooLarge       ErrorCode = 46
	ErrMaxSymbolValueTooSmall       ErrorCode = 48
	ErrStageWrong                   ErrorCode = 60
	ErrDstSizeTooSmall              ErrorCode = 70
	ErrSrcSizeWrong                 ErrorCode = 72
	ErrDstBufferNull                ErrorCode = 74
	ErrNoForwardProgressDestFull    ErrorCode = 80
	ErrNoForwardProgressInputEmpty  ErrorCode = 82
)

// Error is a decoding error carrying the libzstd error code it corresponds to.
type Error struct {
	Code ErrorCode
	msg  string
}

func (e *Error) Error() string { return e.msg }

func newErr(c ErrorCode, format string, a ...any) *Error {
	return &Error{c, "zstd: " + fmt.Sprintf(format, a...)}
}

var (
	errCorrupt  = &Error{ErrCorruptionDetected, "zstd: corrupt input"}
	errSrcSize  = &Error{ErrSrcSizeWrong, "zstd: source size wrong (truncated input)"}
	errDictBad  = &Error{ErrDictionaryCorrupted, "zstd: corrupted dictionary"}
	errDictID   = &Error{ErrDictionaryWrong, "zstd: dictionary mismatch"}
	errChecksum = &Error{ErrChecksumWrong, "zstd: checksum mismatch"}
	errPrefix   = &Error{ErrPrefixUnknown, "zstd: bad magic"}
	errWindow   = &Error{ErrFrameParameterWindowTooLarge, "zstd: window too large"}
	errReserved = &Error{ErrFrameParameterUnsupported, "zstd: unsupported frame parameter (reserved bit)"}
)

// codeOf returns the error code of err (ErrGeneric for foreign errors, 0 for nil).
func codeOf(err error) ErrorCode {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ErrGeneric
}

// debugErrs makes corrupt() record the source line of the failure (tests only).
var debugErrs bool

// corrupt returns the corruption error; with debugErrs it names the caller.
func corrupt() *Error {
	if debugErrs {
		_, f, l, _ := runtime.Caller(1)
		return newErr(ErrCorruptionDetected, "corrupt input at %s:%d", f, l)
	}
	return errCorrupt
}
