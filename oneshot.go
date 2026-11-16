package zstd

// Encode appends a frame of src to dst at the default level (3), without a dictionary; safe for concurrent use.
func Encode(dst, src []byte) []byte { return (*Dict)(nil).Encode(dst, src) }

// EncodeLevel is Encode at a libzstd compression level (-50 to 22; 0 is the default, 3).
func EncodeLevel(dst, src []byte, level int) []byte { return (*Dict)(nil).EncodeLevel(dst, src, level) }

// Decode appends the decompressed content of the frames in src to dst, without a dictionary; safe for concurrent use. Use a
// Decoder for limits and options.
func Decode(dst, src []byte) ([]byte, error) { return (*Dict)(nil).Decode(dst, src) }
