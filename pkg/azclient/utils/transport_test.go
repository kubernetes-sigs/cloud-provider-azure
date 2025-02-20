/*
Copyright 2025 The Kubernetes Authors.

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

package utils

import (
	"crypto/tls"
	"testing"
	"time"
)

func TestDefaultTransport(t *testing.T) {
	if DefaultTransport == nil {
		t.Fatal("DefaultTransport is nil")
	}

	if DefaultTransport.DialContext == nil {
		t.Error("DialContext is nil")
	}

	if !DefaultTransport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 is false")
	}

	if DefaultTransport.MaxIdleConns != 100 {
		t.Errorf("Expected MaxIdleConns to be 100, got %d", DefaultTransport.MaxIdleConns)
	}

	if DefaultTransport.MaxConnsPerHost != 100 {
		t.Errorf("Expected MaxConnsPerHost to be 100, got %d", DefaultTransport.MaxConnsPerHost)
	}

	if DefaultTransport.IdleConnTimeout != 90*time.Second {
		t.Errorf("Expected IdleConnTimeout to be 90s, got %v", DefaultTransport.IdleConnTimeout)
	}

	if DefaultTransport.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("Expected TLSHandshakeTimeout to be 10s, got %v", DefaultTransport.TLSHandshakeTimeout)
	}

	if DefaultTransport.ExpectContinueTimeout != 1*time.Second {
		t.Errorf("Expected ExpectContinueTimeout to be 1s, got %v", DefaultTransport.ExpectContinueTimeout)
	}

	if DefaultTransport.ResponseHeaderTimeout != 60*time.Second {
		t.Errorf("Expected ResponseHeaderTimeout to be 60s, got %v", DefaultTransport.ResponseHeaderTimeout)
	}

	if DefaultTransport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}

	if DefaultTransport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("Expected MinVersion to be TLS1.2, got %v", DefaultTransport.TLSClientConfig.MinVersion)
	}

	if DefaultTransport.TLSClientConfig.Renegotiation != tls.RenegotiateNever {
		t.Errorf("Expected Renegotiation to be RenegotiateNever, got %v", DefaultTransport.TLSClientConfig.Renegotiation)
	}

	// NOTE: http2 transport settings are not exposed hence testing is skipped.
}
