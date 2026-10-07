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
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
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

// Run the real binary so policy overrides in main() cannot escape coverage.
func TestHealthProbeProxyAcceptsHTTP(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "health-probe-proxy")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build health-probe-proxy: %v\n%s", err, output)
	}

	const backendBody = "healthy backend"
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, backendBody)
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)
	_, targetPort, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to get backend port: %v", err)
	}

	// The binary takes a port, not an inherited listener, so release an ephemeral
	// port immediately before starting it.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve proxy port: %v", err)
	}
	addr := reservation.Addr().String()
	_, healthCheckPort, err := net.SplitHostPort(addr)
	if err != nil {
		_ = reservation.Close()
		t.Fatalf("failed to get proxy port: %v", err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatalf("failed to release proxy port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--health-check-port="+healthCheckPort, "--target-port="+targetPort)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start health-probe-proxy: %v", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if t.Failed() {
			t.Logf("health-probe-proxy output:\n%s", logs.String())
		}
	})

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			t.Fatalf("health-probe-proxy exited before becoming ready: %v", waitErr)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for health-probe-proxy: %v", ctx.Err())
		case <-ticker.C:
		}
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			if err := conn.Close(); err != nil {
				t.Fatalf("failed to close readiness connection: %v", err)
			}
			break
		}
	}

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
					t.Fatalf("headerless health probe through binary was rejected: %v", err)
				}
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read health probe response: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200 from backend, got %d: %s", resp.StatusCode, body)
			}
			if string(body) != backendBody {
				t.Fatalf("expected backend response %q, got %q", backendBody, body)
			}
		})
	}
}
