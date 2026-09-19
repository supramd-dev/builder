package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveListenAddr is what turns server.addr / server.port (or their
// MD_BUILDER_ADDR / MD_BUILDER_PORT overrides) into the address the server
// binds, so its precedence is worth pinning down: the port setting wins over
// the port inside the address, and an address without a port takes the
// default one.
func TestResolveListenAddr(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		port    int
		want    string
		wantErr bool
	}{
		{name: "defaults", addr: defaultListenAddr, want: ":8080"},
		{name: "port setting", addr: defaultListenAddr, port: 9000, want: ":9000"},
		{name: "host only", addr: "127.0.0.1", want: "127.0.0.1:8080"},
		{name: "host only with port setting", addr: "127.0.0.1", port: 9000, want: "127.0.0.1:9000"},
		{name: "hostname only", addr: "localhost", want: "localhost:8080"},
		{name: "addr port kept", addr: "127.0.0.1:8000", want: "127.0.0.1:8000"},
		{name: "port setting overrides addr port", addr: "127.0.0.1:8000", port: 9000, want: "127.0.0.1:9000"},
		{name: "all interfaces", addr: "0.0.0.0:9000", want: "0.0.0.0:9000"},
		{name: "ipv6 literal", addr: "::1", want: "[::1]:8080"},
		{name: "ipv6 with port", addr: "[::1]:8000", want: "[::1]:8000"},
		{name: "ipv6 with port setting", addr: "[::1]", port: 9000, want: "[::1]:9000"},
		{name: "ephemeral port", addr: ":0", want: ":0"},
		{name: "empty addr", addr: "", want: ":8080"},

		{name: "port too large", addr: ":8080", port: 70000, wantErr: true},
		{name: "negative port", addr: ":8080", port: -1, wantErr: true},
		{name: "non-numeric port in addr", addr: "localhost:http", wantErr: true},
		{name: "malformed addr", addr: "127.0.0.1:8080:9000", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveListenAddr(tc.addr, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveListenAddr(%q, %d) = %q, want an error", tc.addr, tc.port, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListenAddr(%q, %d): %v", tc.addr, tc.port, err)
			}
			if got != tc.want {
				t.Fatalf("resolveListenAddr(%q, %d) = %q, want %q", tc.addr, tc.port, got, tc.want)
			}
		})
	}
}

func TestListenURL(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":8080", "http://localhost:8080"},
		{"0.0.0.0:9000", "http://localhost:9000"},
		{"[::]:9000", "http://localhost:9000"},
		{"127.0.0.1:8000", "http://127.0.0.1:8000"},
		{"[::1]:8000", "http://[::1]:8000"},
	}
	for _, tc := range cases {
		if got := listenURL(tc.addr); got != tc.want {
			t.Errorf("listenURL(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// resolveDistDir takes the configured directory when there is one, and
// otherwise searches the working directory, so the binary runs from either
// the project root or server/.
func TestResolveDistDir(t *testing.T) {
	t.Chdir(t.TempDir())

	if got := resolveDistDir("/srv/dist"); got != "/srv/dist" {
		t.Errorf("configured dist = %q, want it used as given", got)
	}
	if got := resolveDistDir(""); got != "frontend/dist" {
		t.Errorf("unconfigured dist = %q, want the built-in default when nothing exists", got)
	}

	if err := os.MkdirAll(filepath.Join("..", "frontend", "dist"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := resolveDistDir(""); got != "../frontend/dist" {
		t.Errorf("dist = %q, want the first existing candidate", got)
	}
}
