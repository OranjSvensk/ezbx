// Package ui holds ezbx's terminal banner.
package ui

import (
	"fmt"
	"io"
)

// ANSI colour codes. Callers decide whether the terminal takes them.
const (
	accent = "\033[38;2;36;150;237m" // accent blue (#2496ED), as 24-bit colour
	dim    = "\033[2m"               // faint text for the tagline
	reset  = "\033[0m"               // turns colour off again
)

// Art is the block-letter "EZBX" banner. Edit this text to change the look.
const Art = `███████╗███████╗██████╗ ██╗  ██╗
██╔════╝╚══███╔╝██╔══██╗╚██╗██╔╝
█████╗    ███╔╝ ██████╔╝ ╚███╔╝
██╔══╝   ███╔╝  ██╔══██╗ ██╔██╗
███████╗███████╗██████╔╝██╔╝ ██╗
╚══════╝╚══════╝╚═════╝ ╚═╝  ╚═╝`

// Tagline is the line under the banner, also used in the README.
const Tagline = "boxed-up local AI agents"

// Banner writes the banner and tagline to w, in colour when color is true.
// Plain text otherwise, for terminals that don't take colour codes and for
// NO_COLOR (https://no-color.org).
func Banner(w io.Writer, color bool) {
	if !color {
		fmt.Fprintln(w, Art)
		fmt.Fprintln(w, "  "+Tagline)
		return
	}
	fmt.Fprintln(w, accent+Art+reset)
	fmt.Fprintln(w, "  "+dim+Tagline+reset)
}
