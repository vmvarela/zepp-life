package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// errNoTTY reports that no interactive terminal is available for a hidden
// prompt. Commands translate it into an actionable interaction_required.
var errNoTTY = errors.New("no interactive terminal available")

// platformPromptSecret asks for a secret on the controlling terminal with echo
// turned off. The value comes from the terminal device itself, never from piped
// standard input, and it is never written to standard output or any log.
func platformPromptSecret(message string) (string, error) {
	console, err := openConsole()
	if err != nil {
		return "", errNoTTY
	}
	defer console.Close()

	fmt.Fprint(os.Stderr, message)
	line, err := term.ReadPassword(int(console.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("cannot read from the terminal: %w", err)
	}
	return strings.TrimSpace(string(line)), nil
}

// openConsole opens the controlling terminal. Windows uses the console input
// device; everything else uses /dev/tty.
func openConsole() (*os.File, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile("CONIN$", os.O_RDWR, 0)
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
