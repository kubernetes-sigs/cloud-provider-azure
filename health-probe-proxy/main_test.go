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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
)

func sendProxyHeaderRequest(t *testing.T, addr string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("failed to set connection deadline: %v", err)
	}

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
	return resp
}

func TestProxyListenerAcceptsHTTP(t *testing.T) {
	const responseBody = "healthy"
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, responseBody)
	})
	server := httptest.NewUnstartedServer(mux)
	server.Listener = newProxyListener(server.Listener)
	server.Start()
	t.Cleanup(server.Close)
	addr := server.Listener.Addr().String()

	for _, name := range []string{"headerless", "proxy-header"} {
		t.Run(name, func(t *testing.T) {
			var resp *http.Response
			if name == "proxy-header" {
				resp = sendProxyHeaderRequest(t, addr)
			} else {
				transport := &http.Transport{}
				t.Cleanup(transport.CloseIdleConnections)
				client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
				var err error
				resp, err = client.Get("http://" + addr + "/healthz")
				if err != nil {
					t.Fatalf("headerless health probe was rejected: %v", err)
				}
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read health probe response: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, body)
			}
			if string(body) != responseBody {
				t.Fatalf("expected response %q, got %q", responseBody, body)
			}
		})
	}
}

func TestNewProxyListenerPolicy(t *testing.T) {
	listener := newProxyListener(nil)
	if listener.ConnPolicy == nil {
		t.Fatal("expected an explicit connection policy, not the global default")
	}
	policy, err := listener.ConnPolicy(proxyproto.ConnPolicyOptions{})
	if err != nil {
		t.Fatalf("connection policy returned an error: %v", err)
	}
	if policy != proxyproto.USE {
		t.Fatalf("expected USE policy, got %v", policy)
	}
}
