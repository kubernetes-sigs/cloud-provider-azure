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

package imdstest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

type testResponse struct {
	status int
	body   string
	close  bool
}

// get issues a GET request without using testing.T so that it is safe in worker goroutines.
func get(url string) (testResponse, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return testResponse{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return testResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return testResponse{}, err
	}
	return testResponse{status: resp.StatusCode, body: string(body), close: resp.Close}, nil
}

func bodyHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}
}

func statusHandler(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

func TestServerRouting(t *testing.T) {
	instance := bodyHandler("instance")
	lb := bodyHandler("loadbalancer")
	for _, tc := range []struct {
		desc             string
		instance         http.HandlerFunc
		lb               http.HandlerFunc
		path             string
		expectedStatus   int
		expectedBody     string
		expectedInstance int32
		expectedLB       int32
	}{
		{
			desc:             "instance path should use the instance handler",
			instance:         instance,
			lb:               lb,
			path:             consts.ImdsInstanceURI,
			expectedStatus:   http.StatusOK,
			expectedBody:     "instance",
			expectedInstance: 1,
		},
		{
			desc:           "load balancer path should use the load balancer handler",
			instance:       instance,
			lb:             lb,
			path:           consts.ImdsLoadBalancerURI,
			expectedStatus: http.StatusOK,
			expectedBody:   "loadbalancer",
			expectedLB:     1,
		},
		{
			desc:           "unknown path should return 404 without counting",
			instance:       instance,
			lb:             lb,
			path:           "/metadata/unknown",
			expectedStatus: http.StatusNotFound,
		},
		{
			desc:             "nil instance handler should return 404 and count the request",
			lb:               lb,
			path:             consts.ImdsInstanceURI,
			expectedStatus:   http.StatusNotFound,
			expectedInstance: 1,
		},
		{
			desc:           "nil load balancer handler should return 404 and count the request",
			instance:       instance,
			path:           consts.ImdsLoadBalancerURI,
			expectedStatus: http.StatusNotFound,
			expectedLB:     1,
		},
	} {
		for _, closeConnections := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/closeConnections=%t", tc.desc, closeConnections), func(t *testing.T) {
				s := NewServer(t, tc.instance, tc.lb, closeConnections)

				resp, err := get(s.URL + tc.path)
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, tc.expectedStatus, resp.status)
				assert.Equal(t, tc.expectedBody, resp.body)
				assert.Equal(t, closeConnections, resp.close, "Connection: close should only be set when enabled")
				assert.Equal(t, tc.expectedInstance, s.InstanceCalls())
				assert.Equal(t, tc.expectedLB, s.LBCalls())
			})
		}
	}
}

func TestServerSetHandlersAndResetCalls(t *testing.T) {
	s := NewServer(t, bodyHandler("instance-1"), bodyHandler("loadbalancer-1"), false)

	resp, err := get(s.URL + consts.ImdsInstanceURI)
	assert.NoError(t, err)
	assert.Equal(t, "instance-1", resp.body)
	resp, err = get(s.URL + consts.ImdsLoadBalancerURI)
	assert.NoError(t, err)
	assert.Equal(t, "loadbalancer-1", resp.body)

	s.SetInstanceHandler(bodyHandler("instance-2"))
	s.SetLoadBalancerHandler(statusHandler(http.StatusServiceUnavailable))

	resp, err = get(s.URL + consts.ImdsInstanceURI)
	assert.NoError(t, err)
	assert.Equal(t, "instance-2", resp.body)
	resp, err = get(s.URL + consts.ImdsLoadBalancerURI)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Equal(t, int32(2), s.InstanceCalls())
	assert.Equal(t, int32(2), s.LBCalls())

	s.ResetCalls()
	assert.Equal(t, int32(0), s.InstanceCalls())
	assert.Equal(t, int32(0), s.LBCalls())

	_, err = get(s.URL + consts.ImdsLoadBalancerURI)
	assert.NoError(t, err)
	assert.Equal(t, int32(0), s.InstanceCalls())
	assert.Equal(t, int32(1), s.LBCalls())
}

func TestServerConcurrentHandlerUpdates(t *testing.T) {
	const requests = 20
	s := NewServer(t, bodyHandler("instance"), bodyHandler("loadbalancer"), false)

	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.SetInstanceHandler(bodyHandler("instance"))
			s.SetLoadBalancerHandler(bodyHandler("loadbalancer"))
			_ = s.LBCalls()
		}()
		go func() {
			defer wg.Done()
			resp, err := get(s.URL + consts.ImdsLoadBalancerURI)
			if err != nil {
				errs <- err
				return
			}
			if resp.status != http.StatusOK || resp.body != "loadbalancer" {
				errs <- fmt.Errorf("unexpected response: status %d, body %q", resp.status, resp.body)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(requests), s.LBCalls())
	assert.Equal(t, int32(0), s.InstanceCalls())
}

func TestServerClosedOnCleanup(t *testing.T) {
	var url string
	t.Run("server", func(t *testing.T) {
		s := NewServer(t, bodyHandler("instance"), nil, false)
		url = s.URL
		resp, err := get(url + consts.ImdsInstanceURI)
		assert.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.status)
	})

	_, err := get(url + consts.ImdsInstanceURI)
	assert.Error(t, err, "the server should be closed when the test that created it completes")
}
