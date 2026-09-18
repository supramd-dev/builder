package main

import "testing"

// resolveListenAddr is what turns -addr/-port into the address the server
// binds, so its precedence is worth pinning down: -port wins over the port
// inside -addr, and an address without a port takes the default one.
func TestResolveListenAddr(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		port    int
		want    string
		wantErr bool
	}{
		{name: "defaults", addr: defaultListenAddr, want: ":8080"},
		{name: "port flag", addr: defaultListenAddr, port: 9000, want: ":9000"},
		{name: "host only", addr: "127.0.0.1", want: "127.0.0.1:8080"},
		{name: "host only with port flag", addr: "127.0.0.1", port: 9000, want: "127.0.0.1:9000"},
		{name: "hostname only", addr: "localhost", want: "localhost:8080"},
		{name: "addr port kept", addr: "127.0.0.1:8000", want: "127.0.0.1:8000"},
		{name: "port flag overrides addr port", addr: "127.0.0.1:8000", port: 9000, want: "127.0.0.1:9000"},
		{name: "all interfaces", addr: "0.0.0.0:9000", want: "0.0.0.0:9000"},
		{name: "ipv6 literal", addr: "::1", want: "[::1]:8080"},
		{name: "ipv6 with port", addr: "[::1]:8000", want: "[::1]:8000"},
		{name: "ipv6 with port flag", addr: "[::1]", port: 9000, want: "[::1]:9000"},
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

// The container image is configured through MD_BUILDER_ADDR/MD_BUILDER_PORT,
// with the flags still overriding them.
func TestDefaultListenFlags(t *testing.T) {
	cases := []struct {
		name     string
		envAddr  string
		envPort  string
		wantAddr string
		wantPort int
		wantErr  bool
	}{
		{name: "unset", wantAddr: ":8080"},
		{name: "env addr", envAddr: "127.0.0.1:9000", wantAddr: "127.0.0.1:9000"},
		{name: "env port", envPort: "9000", wantAddr: ":8080", wantPort: 9000},
		{name: "both", envAddr: "0.0.0.0", envPort: "9000", wantAddr: "0.0.0.0", wantPort: 9000},
		{name: "port zero", envPort: "0", wantAddr: ":8080"},
		{name: "not a number", envPort: "eighty", wantErr: true},
		{name: "out of range", envPort: "70000", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envAddr, tc.envAddr)
			t.Setenv(envPort, tc.envPort)
			addr, port, err := defaultListenFlags()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %s=%s", envPort, tc.envPort)
				}
				return
			}
			if err != nil {
				t.Fatalf("defaultListenFlags: %v", err)
			}
			if addr != tc.wantAddr || port != tc.wantPort {
				t.Fatalf("got (%q, %d), want (%q, %d)", addr, port, tc.wantAddr, tc.wantPort)
			}
		})
	}
}
