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

// Package imdstest provides a local Azure Instance Metadata Service server for tests.
package imdstest

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// Server is a local IMDS test server. It routes the instance and load balancer
// metadata paths to replaceable handlers and counts requests per path.
type Server struct {
	// URL is the base URL of the server.
	URL string

	server           *httptest.Server
	closeConnections bool

	mu       sync.Mutex
	instance http.HandlerFunc
	lb       http.HandlerFunc

	instanceCalls atomic.Int32
	lbCalls       atomic.Int32
}

// NewServer starts a Server that is closed when the test completes.
//
// Requests to the instance and load balancer metadata paths are counted and passed
// to the corresponding handler with the original ResponseWriter; a nil handler
// responds with 404. Requests to other paths respond with 404 and are not counted.
// When closeConnections is true, every response carries "Connection: close" so that
// connection reuse cannot hide requests from the counters.
func NewServer(t testing.TB, instance, lb http.HandlerFunc, closeConnections bool) *Server {
	t.Helper()
	s := &Server{closeConnections: closeConnections, instance: instance, lb: lb}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.server.Close)
	s.URL = s.server.URL
	return s
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.closeConnections {
		w.Header().Set("Connection", "close")
	}
	var handler http.HandlerFunc
	switch r.URL.Path {
	case consts.ImdsInstanceURI:
		s.instanceCalls.Add(1)
		s.mu.Lock()
		handler = s.instance
		s.mu.Unlock()
	case consts.ImdsLoadBalancerURI:
		s.lbCalls.Add(1)
		s.mu.Lock()
		handler = s.lb
		s.mu.Unlock()
	}
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	handler(w, r)
}

// SetInstanceHandler replaces the handler for subsequent instance metadata requests.
func (s *Server) SetInstanceHandler(handler http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instance = handler
}

// SetLoadBalancerHandler replaces the handler for subsequent load balancer metadata requests.
func (s *Server) SetLoadBalancerHandler(handler http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lb = handler
}

// InstanceCalls returns the number of instance metadata requests since the last reset.
func (s *Server) InstanceCalls() int32 {
	return s.instanceCalls.Load()
}

// LBCalls returns the number of load balancer metadata requests since the last reset.
func (s *Server) LBCalls() int32 {
	return s.lbCalls.Load()
}

// ResetCalls resets both request counters to zero.
func (s *Server) ResetCalls() {
	s.instanceCalls.Store(0)
	s.lbCalls.Store(0)
}
