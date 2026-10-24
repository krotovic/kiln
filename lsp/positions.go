package lsp

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// text is a document's source with its line starts, to turn the language's byte offsets into LSP positions
// (lines, and characters counted in UTF-16 code units) and back.
type text struct {
	src   string
	lines []int // byte offset of each line's start
}

func newText(src string) *text {
	t := &text{src: src, lines: []int{0}}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			t.lines = append(t.lines, i+1)
		}
	}
	return t
}

// position turns a byte offset into a position.
func (t *text) position(offset int) Position {
	offset = max(0, min(offset, len(t.src)))
	line := sort.Search(len(t.lines), func(i int) bool { return t.lines[i] > offset }) - 1
	char := 0
	for _, r := range t.src[t.lines[line]:offset] {
		char += utf16Len(r)
	}
	return Position{Line: line, Character: char}
}

// offset turns a position into a byte offset; past the end of a line is its end.
func (t *text) offset(p Position) int {
	if p.Line < 0 {
		return 0
	}
	if p.Line >= len(t.lines) {
		return len(t.src)
	}
	start := t.lines[p.Line]
	end := len(t.src)
	if p.Line+1 < len(t.lines) {
		end = t.lines[p.Line+1] - 1
	}
	i, char := start, 0
	for i < end && char < p.Character {
		r, size := utf8.DecodeRuneInString(t.src[i:])
		char += utf16Len(r)
		i += size
	}
	return i
}

// rangeOf is the range of the bytes [start, end).
func (t *text) rangeOf(start, end int) Range {
	return Range{Start: t.position(start), End: t.position(end)}
}

// lineCol turns a 1-based line and byte column (as diagnostics have them) into a byte offset.
func (t *text) lineCol(line, col int) int {
	if line < 1 || line > len(t.lines) {
		return len(t.src)
	}
	return min(t.lines[line-1]+col-1, len(t.src))
}

// word is the range a diagnostic at offset underlines: the name or number there, or one character.
func (t *text) word(offset int) (int, int) {
	end := offset
	for end < len(t.src) && isWordByte(t.src[end]) {
		end++
	}
	if end == offset {
		if offset < len(t.src) && t.src[offset] != '\n' {
			_, size := utf8.DecodeRuneInString(t.src[offset:])
			end = offset + size
		} else if offset > 0 && !strings.HasSuffix(t.src[:offset], "\n") {
			return offset - 1, offset // at the end of a line: the character before
		}
	}
	return offset, end
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}
