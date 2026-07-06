package main

import "testing"

func TestHostIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8079", true},
		{"localhost:8079", true},
		{"[::1]:8079", true},
		{"0.0.0.0:8079", false},
		{":8079", false},
		{"10.0.0.5:8079", false},
		{"example.com:8079", false},
		{"not-a-valid-addr", false},
	}
	for _, c := range cases {
		if got := hostIsLoopback(c.addr); got != c.want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
