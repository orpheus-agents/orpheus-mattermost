package main

import (
	"testing"
)

func TestHealthURLFollowsListener(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{":8090", "http://127.0.0.1:8090/healthz"},
		{"0.0.0.0:8090", "http://127.0.0.1:8090/healthz"},
		{"[::]:8090", "http://[::1]:8090/healthz"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000/healthz"},
	} {
		t.Setenv("LISTEN_ADDR", tc.listen)
		if got := healthURL(); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}
