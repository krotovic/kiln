package lsp

import (
	"strings"
	"testing"
)

func TestPositions(t *testing.T) {
	tx := newText("let é = \"😀x\"\nnext")
	// é is 2 bytes and 1 UTF-16 unit; 😀 is 4 bytes and 2 units
	if p := tx.position(strings.Index(tx.src, "x\"")); p != (Position{Line: 0, Character: 11}) {
		t.Fatalf("position = %+v", p)
	}
	if off := tx.offset(Position{Line: 0, Character: 11}); tx.src[off] != 'x' {
		t.Fatalf("offset = %d", off)
	}
	if off := tx.offset(Position{Line: 1, Character: 99}); off != len(tx.src) {
		t.Fatalf("past the end of a line = %d", off)
	}
	if start, end := tx.word(4); tx.src[start:end] != "é" && end-start != 2 {
		t.Fatalf("word = %q", tx.src[start:end])
	}
}
