package main

import "testing"

func TestValidateServeAddress(t *testing.T) {
	for _, test := range []struct {
		address   string
		insecure  bool
		wantError bool
	}{
		{"", true, false},
		{"127.0.0.1:8082", true, false},
		{"localhost:8082", true, false},
		{"[::1]:8082", true, false},
		{":8082", true, true},
		{"0.0.0.0:8082", true, true},
		{"example.com:8082", true, true},
		{"0.0.0.0:8082", false, false},
		{"bad address", false, true},
	} {
		if err := validateServeAddress(test.address, test.insecure); (err != nil) != test.wantError {
			t.Errorf("address=%q insecure=%v: %v", test.address, test.insecure, err)
		}
	}
}

func TestHealthcheckURL(t *testing.T) {
	for _, test := range []struct {
		addr string
		want string
	}{
		{"", "http://127.0.0.1:8082/healthz"},
		{"0.0.0.0:8082", "http://127.0.0.1:8082/healthz"},
		{":9000", "http://127.0.0.1:9000/healthz"},
		{"[::1]:8082", "http://[::1]:8082/healthz"},
	} {
		t.Setenv("OVERLOAD_ADDR", test.addr)
		if got, err := healthcheckURL(); err != nil || got != test.want {
			t.Errorf("addr=%q got=%q err=%v", test.addr, got, err)
		}
	}
}
