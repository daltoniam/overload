package main

import (
	"errors"
	"fmt"
	"net"
)

func validateServeAddress(address string, insecure bool) error {
	if address == "" {
		address = "127.0.0.1:8082"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid OVERLOAD_ADDR: %w", err)
	}
	if insecure && (host == "" || host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return errors.New("OVERLOAD_UI_INSECURE requires a loopback OVERLOAD_ADDR")
	}
	return nil
}
