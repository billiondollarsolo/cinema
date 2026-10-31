package task

import (
	"errors"
	"strings"
	"testing"
)

// Lab (hono on macOS 14, amd64 emulated): `bun --version` died with exit 132 and
// nothing said why.
func TestSigillHint(t *testing.T) {
	if h := sigillHint(errors.New("exit status 132")); !strings.Contains(h, "illegal instruction") {
		t.Errorf("exit 132: hint %q", h)
	}
	if h := sigillHint(errors.New("exit status 1")); h != "" {
		t.Errorf("exit 1: hint %q", h)
	}
}
