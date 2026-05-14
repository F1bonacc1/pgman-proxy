package output

import (
	"io"
	"os"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
)

// Color owns the green/yellow/red severity palette. The actual
// fatih/color package honours NO_COLOR and TTY detection out of the
// box; we add the --no-color override and a forced-on switch for the
// "render to ANSI golden file" test mode.
type Color struct {
	disabled bool
}

// NewColor decides whether to emit ANSI escapes based on:
//
//  1. forceNoColor (--no-color flag) → off.
//  2. NO_COLOR env var (per no-color.org) → off.
//  3. stdout not a TTY → off.
//  4. Otherwise → on.
func NewColor(forceNoColor bool, out io.Writer) *Color {
	if forceNoColor {
		color.NoColor = true
		return &Color{disabled: true}
	}
	if os.Getenv("NO_COLOR") != "" {
		color.NoColor = true
		return &Color{disabled: true}
	}
	// fatih/color auto-disables when stdout isn't a tty, but only on
	// its package-level state; honour stdout-redirection cases
	// (-o json | jq, golden-file capture).
	if f, ok := out.(*os.File); ok && !isatty.IsTerminal(f.Fd()) {
		color.NoColor = true
		return &Color{disabled: true}
	}
	color.NoColor = false
	return &Color{disabled: false}
}

// Disabled reports whether ANSI emission is suppressed.
func (c *Color) Disabled() bool { return c.disabled }

// Green/Yellow/Red emit colorized strings; in --no-color mode they
// return the input verbatim so callers can compose without branching.
func (c *Color) Green(s string) string  { return color.GreenString("%s", s) }
func (c *Color) Yellow(s string) string { return color.YellowString("%s", s) }
func (c *Color) Red(s string) string    { return color.RedString("%s", s) }
func (c *Color) Bold(s string) string {
	if c.disabled {
		return s
	}
	return color.New(color.Bold).Sprint(s)
}

// ForceDisable forces disabled mode (used by tests). Idempotent.
func (c *Color) ForceDisable() {
	c.disabled = true
	color.NoColor = true
}
