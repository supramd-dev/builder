package main

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// readPasswordFromTerminal prompts and reads a password from the controlling
// terminal without echoing it. Returns an error if stdin is not a TTY.
func readPasswordFromTerminal(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr) // newline after the hidden input
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(b), nil
}
