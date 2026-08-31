package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeForTerminal removes ANSI escape sequences and control characters
// from a string to prevent terminal injection attacks. External tracker content
// (issue titles, descriptions) should be sanitized before terminal display.
//
// Preserves printable UTF-8 characters including common punctuation and emoji.
// Strips: ANSI CSI sequences (\x1b[...), OSC sequences (\x1b]...\x07),
// other escape sequences, and C0/C1 control characters (except \n and \t).
func SanitizeForTerminal(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	n := len(s)
	for i := 0; i < n; {
		if next, handled := sanitizeEscape(s, i); handled {
			i = next
			continue
		}
		if next, handled := sanitizeTerminalControl(&b, s, i); handled {
			i = next
			continue
		}
		if next, handled := appendTerminalRune(&b, s, i); handled {
			i = next
			continue
		}
		i++
	}

	return b.String()
}

func sanitizeEscape(s string, i int) (int, bool) {
	if s[i] != '\x1b' {
		return i, false
	}
	if i+1 >= len(s) {
		return i + 1, true
	}
	switch s[i+1] {
	case '[':
		return skipCSI(s, i+2), true
	case ']':
		return skipOSC(s, i+2), true
	default:
		return i + 2, true
	}
}

func skipCSI(s string, i int) int {
	n := len(s)
	for i < n && s[i] >= 0x20 && s[i] <= 0x3F {
		i++
	}
	if i < n && s[i] >= 0x40 && s[i] <= 0x7E {
		i++
	}
	return i
}

func skipOSC(s string, i int) int {
	n := len(s)
	for i < n {
		if s[i] == '\x07' {
			return i + 1
		}
		if s[i] == '\x1b' && i+1 < n && s[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return i
}

func sanitizeTerminalControl(b *strings.Builder, s string, i int) (int, bool) {
	ch := s[i]
	if ch == '\n' || ch == '\t' {
		b.WriteByte(ch)
		return i + 1, true
	}
	if ch < 0x20 || ch == 0x7F {
		return i + 1, true
	}
	return i, false
}

func appendTerminalRune(b *strings.Builder, s string, i int) (int, bool) {
	ch := s[i]
	r := rune(ch)
	size := 1
	if ch >= 0x80 {
		r, size = utf8.DecodeRuneInString(s[i:])
		if r == unicode.ReplacementChar && size == 1 {
			return i, false
		}
		if r >= 0x80 && r <= 0x9F {
			return i + size, true
		}
	}
	b.WriteString(s[i : i+size])
	return i + size, true
}
