// Package zst is a thin wrapper over klauspost zstd for compressing raw
// transcript lines.
package zst

import "github.com/klauspost/compress/zstd"

var (
	enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ = zstd.NewReader(nil)
)

// Compress returns the zstd frame for b. Safe for concurrent use.
func Compress(b []byte) []byte { return enc.EncodeAll(b, nil) }

// Decompress reverses Compress. An empty input yields an empty output
// (blank transcript lines are stored as empty blobs).
func Decompress(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return []byte{}, nil
	}
	return dec.DecodeAll(b, nil)
}
