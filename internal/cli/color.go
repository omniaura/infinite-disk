package cli

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Use the terminal's own palette, not a forced light/dark theme. Every colored
// state also has a text label; redirected output stays plain in auto mode.
type tone string

const (
	toneHeading tone = "1;36"
	toneReady   tone = "32"
	toneWarning tone = "33"
	toneDanger  tone = "1;31"
	tonePrompt  tone = "1;36"
)

type terminalWriter struct {
	io.Writer
	color bool
}

func terminalOutput(w io.Writer, mode string, json bool) io.Writer {
	// A Cobra command may be executed more than once by an embedding host.
	if previous, ok := w.(*terminalWriter); ok {
		w = previous.Writer
	}
	enabled := false
	if !json {
		switch mode {
		case "always":
			enabled = true
		case "auto":
			if file, ok := w.(interface{ Fd() uintptr }); ok {
				enabled = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(file.Fd()))
			}
		}
	}
	return &terminalWriter{Writer: w, color: enabled}
}

// Style the output writer, not the input text: wrapping and sanitizing happen
// before ANSI codes are added, so escapes never affect widths or bypass plain().
func styled(w io.Writer, color tone) io.Writer {
	if terminal, ok := w.(*terminalWriter); ok && terminal.color {
		return toneWriter{Writer: terminal.Writer, tone: color}
	}
	return w
}

type toneWriter struct {
	io.Writer
	tone tone
}

func (w toneWriter) Write(p []byte) (int, error) {
	// Reset before waiting for input, returning, or printing the next field.
	_, err := fmt.Fprintf(w.Writer, "\x1b[%sm%s\x1b[0m", w.tone, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
