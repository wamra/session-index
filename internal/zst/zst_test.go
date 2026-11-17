package zst

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, in := range [][]byte{[]byte("hello hello hello"), {}, bytes.Repeat([]byte("x"), 1<<20)} {
		out, err := Decompress(Compress(in))
		if err != nil || !bytes.Equal(in, out) {
			t.Fatalf("round trip failed: err=%v", err)
		}
	}
	if out, err := Decompress(nil); err != nil || len(out) != 0 {
		t.Fatalf("empty: %v %v", out, err)
	}
}
