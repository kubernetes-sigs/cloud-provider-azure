/*
Copyright 2023 The Kubernetes Authors.

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

package recording

import (
	"fmt"
	"net/http"
	"sync"
)

var requestHeadersToRemove = []string{
	// remove all Authorization headers from stored requests
	"Authorization",

	// Not needed, adds to diff churn:
	"User-Agent",
}

var responseHeadersToRemove = []string{
	// Request IDs
	"X-Ms-Arm-Service-Request-Id",
	"X-Ms-Correlation-Request-Id",
	"X-Ms-Request-Id",
	"X-Ms-Routing-Request-Id",
	"X-Ms-Client-Request-Id",
	"Client-Request-Id",

	// Quota limits
	"X-Ms-Ratelimit-Remaining-Subscription-Deletes",
	"X-Ms-Ratelimit-Remaining-Subscription-Reads",
	"X-Ms-Ratelimit-Remaining-Subscription-Writes",

	// Not needed, adds to diff churn
	"Date",

	// Causes client to delay long time
	"Retry-After",
}

// Wraps an inner HTTP roundtripper to add a
// counter for duplicated request URIs. This
// is then used to match up requests in the recorder
// - it is needed as we have multiple requests with
// the same Request URL and it will return the first
// one that matches.
type requestCounter struct {
	inner http.RoundTripper

	countsMutex sync.Mutex
	counts      map[string]uint32
}

var _ http.RoundTripper = &requestCounter{}

func addCountHeader(inner http.RoundTripper) *requestCounter {
	return &requestCounter{
		inner:       inner,
		counts:      make(map[string]uint32),
		countsMutex: sync.Mutex{},
	}
}

const countHeader = "TEST-REQUEST-ATTEMPT"

func (rt *requestCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + ":" + req.URL.String()
	rt.countsMutex.Lock()
	count := rt.counts[key]
	rt.counts[key] = count + 1
	rt.countsMutex.Unlock()
	req.Header.Set(countHeader, fmt.Sprintf("%d", count))
	return rt.inner.RoundTrip(req)
}
