package main

import (
	"fmt"
	"io"
	"os"

	"github.com/OranjSvensk/ezbx/internal/ui"
)

// showBanner prints the banner when w is a terminal, so piped or scripted
// output stays plain. Colour is used unless NO_COLOR is set or the terminal
// won't take colour codes.
func showBanner(w io.Writer) {
	f, ok := w.(*os.File)
	if !ok || !isTerminal(f) {
		return
	}
	ui.Banner(f, os.Getenv("NO_COLOR") == "" && enableColor(f))
	fmt.Fprintln(f)
}
