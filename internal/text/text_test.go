package text

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOneLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"only whitespace", " \n\t\r\n ", ""},
		{"plain", "hello world", "hello world"},
		{"newlines", "a\nb\r\nc", "a b c"},
		{"whitespace controls", "a\tb\vc\fd", "a b c d"},
		{"runs collapse", "  a \n\n  b\t\t c  ", "a b c"},
		{"unicode spaces", "a b c　d", "a b c d"},
		{"multi-byte kept", "你好\n世界", "你好 世界"},
		// strings.Fields only splits on whitespace, so other control bytes are kept as is.
		{"non-whitespace control kept", "a\x1bb\x00c", "a\x1bb\x00c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OneLine(tt.in); got != tt.want {
				t.Fatalf("OneLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestOneLineVeryLongInput(t *testing.T) {
	in := strings.Repeat("word\n\t ", 100000)
	got := OneLine(in)
	want := strings.TrimSuffix(strings.Repeat("word ", 100000), " ")
	if got != want {
		t.Fatalf("got %d bytes, want %d bytes", len(got), len(want))
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Fatal("result still contains newline or tab")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"empty zero limit", "", 0, ""},
		{"shorter than limit", "abc", 5, "abc"},
		{"exactly the limit", "abc", 3, "abc"},
		{"cut ascii", "abcdef", 3, "abc…"},
		{"zero limit", "abc", 0, "…"},
		{"cut at rune boundary", "你好世界", 2, "你好…"},
		{"multi-byte exactly the limit", "你好世界", 4, "你好世界"},
		{"mixed width runes", "a你b好c", 3, "a你b…"},
		{"four-byte runes", "😀😁😂😃", 2, "😀😁…"},
		{"newline counts as a rune", "ab\ncd", 3, "ab\n…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.in, tt.n)
			if got != tt.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result %q is not valid UTF-8", got)
			}
		})
	}
}

func TestTruncateVeryLongInput(t *testing.T) {
	in := strings.Repeat("世", 1_000_000)
	got := Truncate(in, 10)
	if want := strings.Repeat("世", 10) + "…"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Truncate(in, 1_000_000); got != in {
		t.Fatal("input at the limit must be returned unchanged")
	}
}
