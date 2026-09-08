package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// adduserSubcommand registers a new user via the command line.
//
// Usage:
//
//	md-builder adduser -username <name> -email <addr> [-password <pw>]
//
// If -password is omitted the password is read interactively from the
// controlling terminal without echoing it. Stdin is read as a fallback when
// there is no TTY (e.g. piping from another process).
func adduserSubcommand() int {
	fs := flag.NewFlagSet("adduser", flag.ContinueOnError)
	username := fs.String("username", "", "username (required)")
	email := fs.String("email", "", "email address (required)")
	password := fs.String("password", "", "password (read interactively if omitted)")
	dsn := fs.String("dsn", defaultDSN(), "database DSN (sqlite path or postgres URL)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}

	*username = strings.TrimSpace(*username)
	*email = strings.TrimSpace(*email)
	if *username == "" || *email == "" {
		fmt.Fprintln(os.Stderr, "error: -username and -email are required")
		fs.Usage()
		return 2
	}

	if *password == "" {
		p, err := readPassword("Enter password: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read password: %v\n", err)
			return 1
		}
		*password = p
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "error: password must not be empty")
		return 2
	}

	s, err := store.Open(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer s.Close()

	// Check for an existing username via count to avoid logging a noisy
	// "record not found" for the common case.
	var count int64
	if err := s.DB.Model(&store.User{}).Where("username = ?", *username).Count(&count).Error; err != nil {
		fmt.Fprintf(os.Stderr, "error: lookup user: %v\n", err)
		return 1
	}
	if count > 0 {
		fmt.Fprintf(os.Stderr, "error: username %q already exists\n", *username)
		return 1
	}

	hash, err := auth.HashPassword(*password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: hash password: %v\n", err)
		return 1
	}
	if err := s.CreateUser(&store.User{
		Username:     *username,
		Email:        *email,
		PasswordHash: hash,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: create user: %v\n", err)
		return 1
	}
	fmt.Printf("created user %q (%s)\n", *username, *email)
	return 0
}

// readPassword reads a password from the TTY without echo when available, and
// falls back to reading a line from stdin otherwise.
func readPassword(prompt string) (string, error) {
	if pwd, err := readPasswordFromTerminal(prompt); err == nil {
		return pwd, nil
	}
	// Fallback: read a line from stdin (e.g. echo "pw" | md-builder adduser).
	line, err := readLine(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func readLine(r io.Reader) (string, error) {
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			b = append(b, buf[0])
		}
		if err != nil {
			if len(b) > 0 {
				break
			}
			return "", err
		}
	}
	return string(b), nil
}
