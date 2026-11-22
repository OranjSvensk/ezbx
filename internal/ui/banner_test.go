package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestBanner(t *testing.T) {
	var plain, color bytes.Buffer
	Banner(&plain, false)
	Banner(&color, true)
	if strings.Contains(plain.String(), "\033") {
		t.Error("the plain banner must carry no colour codes")
	}
	if !strings.HasPrefix(plain.String(), Art+"\n") || !strings.Contains(plain.String(), Tagline) {
		t.Errorf("plain banner:\n%s", plain.String())
	}
	if !strings.HasPrefix(color.String(), accent) || !strings.Contains(color.String(), Art) || strings.Count(color.String(), reset) != 2 {
		t.Errorf("colour banner: %q", color.String())
	}
	if n := len(strings.Split(Art, "\n")); n != 6 {
		t.Errorf("the art has %d lines, want 6", n)
	}
}
