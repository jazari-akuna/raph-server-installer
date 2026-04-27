// launcher_test.go — SSRF allow-list coverage for isPrivateIP.
//
// Regression test for two bypasses fixed alongside this file:
//   1. 0.0.0.0 — Linux routes outbound packets bound for 0.0.0.0 to the
//      loopback interface, so an icon-fetch URL whose host resolves to
//      0.0.0.0 hits a localhost service.
//   2. ::ffff:127.0.0.1 — IPv4-mapped IPv6. Parsed by net.ParseIP as a
//      v6 address that does NOT fall inside any v4 CIDR (127.0.0.0/8 etc.)
//      but whose underlying socket destination is 127.0.0.1.

package main

import (
	"net"
	"testing"
)

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// IPv4 unspecified / loopback / RFC1918 / link-local / CGNAT.
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata service
		{"100.64.0.1", true},

		// IPv6 loopback / unspecified / ULA / link-local / IPv4-mapped variants.
		{"::1", true},
		{"::", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"fe80::1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:0.0.0.0", true},
		{"::ffff:10.1.2.3", true},
		{"::ffff:192.168.0.1", true},

		// Public addresses must NOT be flagged.
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
		{"2606:4700:4700::1111", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.in)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) returned nil", tc.in)
		}
		if got := isPrivateIP(ip); got != tc.want {
			t.Errorf("isPrivateIP(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
