package history

import (
	"errors"
	"regexp"
	"strings"
)

// CallerUnspecified is the caller of a run that did not pass --caller, and of every record
// written before the field existed. It is applied in memory only; stored records are not rewritten.
const CallerUnspecified = "unspecified"

var callerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NormalizeCaller lowercases ASCII letters and checks the canonical caller identifier
// [a-z0-9][a-z0-9._-]{0,63}. Whitespace, non-ASCII characters and the empty string are rejected.
func NormalizeCaller(s string) (string, error) {
	if s == "" {
		return "", errors.New("caller must not be empty")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	n := b.String()
	if !callerPattern.MatchString(n) {
		return "", errors.New("invalid caller: use 1-64 ASCII characters matching [a-z0-9][a-z0-9._-]{0,63} (uppercase is lowercased; no whitespace)")
	}
	return n, nil
}

// CallerOrUnspecified maps a missing caller to CallerUnspecified.
func CallerOrUnspecified(s string) string {
	if s == "" {
		return CallerUnspecified
	}
	return s
}
