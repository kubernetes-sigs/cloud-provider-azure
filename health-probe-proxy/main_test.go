/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
)

// serveProxyListener starts an HTTP server behind a PROXY-protocol listener
// built exactly like main() does (a github.com/pires/go-proxyproto listener with
// no explicit policy, so it relies on proxyproto.DefaultPolicy). It returns the
// address to dial and a cleanup function.
func serveProxyListener(t *testing.T) (string, func()) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	listener := &proxyproto.Listener{Listener: inner}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()

	return inner.Addr().String(), func() { _ = server.Close() }
}

// TestProxyListenerAcceptsHeaderlessHTTP reproduces the regression: Azure load
// balancer health probes send plain HTTP without a PROXY header and must still
// be accepted. go-proxyproto v0.15.0 changed proxyproto.DefaultPolicy to
// REQUIRE, which rejects headerless connections, so this test fails until the
// policy is restored to USE at package scope (e.g. in the main package's init).
func TestProxyListenerAcceptsHeaderlessHTTP(t *testing.T) {
	addr, cleanup := serveProxyListener(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("headerless health probe was rejected: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 for headerless probe, got %d", resp.StatusCode)
	}
}

// TestProxyListenerAcceptsProxyHeader ensures the listener still parses a PROXY
// header when one is present, so proxy-protocol clients keep working regardless
// of the default policy.
func TestProxyListenerAcceptsProxyHeader(t *testing.T) {
	addr, cleanup := serveProxyListener(t)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	header := &proxyproto.Header{
		Version:           1,
		Command:           proxyproto.PROXY,
		TransportProtocol: proxyproto.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 12345},
		DestinationAddr:   &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 10356},
	}
	if _, err := header.WriteTo(conn); err != nil {
		t.Fatalf("failed to write PROXY header: %v", err)
	}
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("PROXY-header request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 for PROXY-header probe, got %d", resp.StatusCode)
	}
}
