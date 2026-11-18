package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDatalessFalseForOrdinaryFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isDataless(p) {
		t.Fatal("ordinary file reported dataless")
	}
	if isDataless(filepath.Join(filepath.Dir(p), "missing")) {
		t.Fatal("missing file reported dataless")
	}
}
