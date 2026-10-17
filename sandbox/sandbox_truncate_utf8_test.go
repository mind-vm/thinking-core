package sandbox

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A log cut inside a multi-byte character must still be valid UTF-8.
func TestTruncateLogsKeepsUTF8(t *testing.T) {
	out := strings.Repeat("Größe ü ", 200)
	for max := 10; max < 60; max++ {
		got := truncateLogs(out, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max %d: invalid UTF-8: %q", max, got)
		}
		if !strings.Contains(got, "bytes of log output omitted") {
			t.Fatalf("max %d: no omission note", max)
		}
	}
}
