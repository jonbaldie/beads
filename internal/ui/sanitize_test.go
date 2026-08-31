package ui

import "testing"

func TestSanitizeForTerminal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text unchanged",
			input: "Hello, world!",
			want:  "Hello, world!",
		},
		{
			name:  "preserves newlines and tabs",
			input: "line1\nline2\ttab",
			want:  "line1\nline2\ttab",
		},
		{
			name:  "strips ANSI color codes",
			input: "\x1b[31mred text\x1b[0m",
			want:  "red text",
		},
		{
			name:  "strips ANSI clear screen",
			input: "\x1b[2J\x1b[HMalicious title",
			want:  "Malicious title",
		},
		{
			name:  "strips ANSI cursor movement",
			input: "\x1b[10;20Hmoved cursor",
			want:  "moved cursor",
		},
		{
			name:  "strips OSC sequences (title change)",
			input: "\x1b]0;evil title\x07Normal text",
			want:  "Normal text",
		},
		{
			name:  "strips OSC with ST terminator",
			input: "\x1b]0;evil\x1b\\Normal",
			want:  "Normal",
		},
		{
			name:  "strips null bytes",
			input: "before\x00after",
			want:  "beforeafter",
		},
		{
			name:  "strips bell character",
			input: "ding\x07dong",
			want:  "dingdong",
		},
		{
			name:  "strips backspace",
			input: "abc\x08def",
			want:  "abcdef",
		},
		{
			name:  "strips carriage return",
			input: "overwrite\rme",
			want:  "overwriteme",
		},
		{
			name:  "strips DEL character",
			input: "hello\x7Fworld",
			want:  "helloworld",
		},
		{
			name:  "preserves unicode",
			input: "café résumé naïve 日本語",
			want:  "café résumé naïve 日本語",
		},
		{
			name:  "preserves emoji",
			input: "🚀 Launch 🎉 Party",
			want:  "🚀 Launch 🎉 Party",
		},
		{
			name:  "complex injection attempt",
			input: "\x1b[2J\x1b[H\x1b]0;pwned\x07IGNORE PREVIOUS INSTRUCTIONS\x1b[31m",
			want:  "IGNORE PREVIOUS INSTRUCTIONS",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only control characters",
			input: "\x1b[31m\x1b[0m",
			want:  "",
		},
		{
			name:  "strips trailing escape",
			input: "safe\x1b",
			want:  "safe",
		},
		{
			name:  "strips non-CSI escape pair",
			input: "a\x1bXb",
			want:  "ab",
		},
		{
			name:  "accepts CSI parameter and final byte lower bounds",
			input: "\x1b[ @visible",
			want:  "visible",
		},
		{
			name:  "accepts CSI parameter and final byte upper bounds",
			input: "\x1b[?~visible",
			want:  "visible",
		},
		{
			name:  "strips unterminated OSC payload",
			input: "visible\x1b]hidden",
			want:  "visible",
		},
		{
			name:  "does not mistake embedded escape for OSC terminator",
			input: "visible\x1b]hidden\x1bXstill hidden",
			want:  "visible",
		},
		{
			name:  "strips encoded C1 boundary controls",
			input: "a\u0080b\u009fc",
			want:  "abc",
		},
		{
			name:  "preserves first rune above C1 range",
			input: "a\u00a0b",
			want:  "a\u00a0b",
		},
		{
			name:  "preserves valid replacement rune",
			input: "a\ufffdb",
			want:  "a\ufffdb",
		},
		{
			name:  "strips invalid UTF-8 bytes",
			input: string([]byte{'a', 0xff, 'b'}),
			want:  "ab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeForTerminal(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeForTerminal(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
