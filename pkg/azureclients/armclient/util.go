/*
Copyright 2022 The Kubernetes Authors.

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

package armclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/go-autorest/autorest"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func NewRateLimitSendDecorater(ratelimiter flowcontrol.RateLimiter, mc *metrics.MetricContext) autorest.SendDecorator {
	return func(s autorest.Sender) autorest.Sender {
		return autorest.SenderFunc(func(r *http.Request) (*http.Response, error) {
			if !ratelimiter.TryAccept() {
				mc.RateLimitedCount()
				return nil, fmt.Errorf("rate limit reached")
			}
			return s.Do(r)
		})
	}
}

func NewThrottledSendDecorater(mc *metrics.MetricContext) autorest.SendDecorator {
	var retryTimer time.Time
	return func(s autorest.Sender) autorest.Sender {
		return autorest.SenderFunc(func(r *http.Request) (*http.Response, error) {
			if retryTimer.After(time.Now()) {
				mc.ThrottledCount()
				return nil, fmt.Errorf("request is throttled")
			}
			resp, err := s.Do(r)
			rerr := retry.GetError(resp, err)
			if rerr.IsThrottled() {
				// Update RetryAfterReader so that no more requests would be sent until RetryAfter expires.
				retryTimer = rerr.RetryAfter
			}
			return resp, err
		})
	}
}

func NewErrorCounterSendDecorator(mc *metrics.MetricContext) autorest.SendDecorator {
	return func(s autorest.Sender) autorest.Sender {
		return autorest.SenderFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := s.Do(r)
			rerr := retry.GetError(resp, err)
			mc.Observe(rerr)
			return resp, err
		})
	}
}

func DoDumpRequest(v klog.Level) autorest.SendDecorator {
	return func(s autorest.Sender) autorest.Sender {

		return autorest.SenderFunc(func(request *http.Request) (*http.Response, error) {
			if request != nil {
				requestDump, err := httputil.DumpRequest(request, true)
				if err != nil {
					klog.Errorf("Failed to dump request: %v", err)
				} else {
					klog.V(v).Infof("Dumping request: %s", string(requestDump))
				}
			}
			return s.Do(request)
		})
	}
}

func WithMetricsSendDecoratorWrapper(prefix, request, resourceGroup, subscriptionID, source string, factory func(mc *metrics.MetricContext) []autorest.SendDecorator) autorest.SendDecorator {
	mc := metrics.NewMetricContext(prefix, request, resourceGroup, subscriptionID, source)
	if factory != nil {
		return func(s autorest.Sender) autorest.Sender {
			return autorest.DecorateSender(s, factory(mc)...)
		}
	}
	return nil
}

// DoHackRegionalRetryDecorator returns an autorest.SendDecorator which performs retry with customizable backoff policy.
func DoHackRegionalRetryDecorator(c *Client) autorest.SendDecorator {
	return func(s autorest.Sender) autorest.Sender {
		return autorest.SenderFunc(func(request *http.Request) (*http.Response, error) {
			response, rerr := s.Do(request)
			if response == nil {
				klog.V(2).Infof("response is empty")
				return response, rerr
			}

			bodyBytes, _ := ioutil.ReadAll(response.Body)
			defer func() {
				response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
			}()

			bodyString := string(bodyBytes)
			trimmed := strings.TrimSpace(bodyString)
			// Hack: retry the regional ARM endpoint in case of ARM traffic split and arm resource group replication is too slow
			// Empty content and 2xx http status code are returned in this case.
			// Issue: https://github.com/kubernetes-sigs/cloud-provider-azure/issues/1296
			emptyResp := (response.ContentLength == 0 || trimmed == "" || trimmed == "{}") && response.StatusCode >= 200 && response.StatusCode < 300
			if !emptyResp {
				if rerr == nil || response.StatusCode == http.StatusNotFound || c.regionalEndpoint == "" {
					return response, rerr
				}

				var body map[string]interface{}
				if e := json.Unmarshal(bodyBytes, &body); e != nil {
					klog.Errorf("Send.sendRequest: error in parsing response body string: %s, Skip retrying regional host", e.Error())
					return response, rerr
				}
				klog.V(5).Infof("Send.sendRequest original response: %s", bodyString)

				err, ok := body["error"].(map[string]interface{})
				if !ok || err["code"] == nil || !strings.EqualFold(err["code"].(string), "ResourceGroupNotFound") {
					klog.V(5).Infof("Send.sendRequest: response body does not contain ResourceGroupNotFound error code. Skip retrying regional host")
					return response, rerr
				}
			}

			currentHost := request.URL.Host
			if request.Host != "" {
				currentHost = request.Host
			}

			if strings.HasPrefix(strings.ToLower(currentHost), c.regionalEndpoint) {
				klog.V(5).Infof("Send.sendRequest: current host %s is regional host. Skip retrying regional host.", html.EscapeString(currentHost))
				return response, rerr
			}

			request.Host = c.regionalEndpoint
			request.URL.Host = c.regionalEndpoint
			klog.V(5).Infof("Send.sendRegionalRequest on ResourceGroupNotFound error. Retrying regional host: %s", html.EscapeString(request.Host))

			regionalResponse, regionalError := s.Do(request)
			// only use the result if the regional request actually goes through and returns 2xx status code, for two reasons:
			// 1. the retry on regional ARM host approach is a hack.
			// 2. the concatenated regional uri could be wrong as the rule is not officially declared by ARM.
			if regionalResponse == nil || regionalResponse.StatusCode > 299 {
				regionalErrStr := ""
				if regionalError != nil {
					regionalErrStr = regionalError.Error()
				}

				klog.V(5).Infof("Send.sendRegionalRequest failed to get response from regional host, error: '%s'. Ignoring the result.", regionalErrStr)
				return response, rerr
			}
			return regionalResponse, regionalError
		})
	}
}

// getRemainingSubscriptionReads checking header for ARM level read count remaining
// this is used to be able to handle client reset if we've hit our limit
func getRemainingSubscriptionReads(resp *http.Response) int {
	if resp == nil || resp.Header == nil || resp.Header.Get(consts.RemainingSubscriptionReadsHeaderKey) == "" {
		// 12000 because this is the default upper limit for subscription reads in ARM
		return 12000
	}

	remainingReads, _ := strconv.Atoi(resp.Header.Get(consts.RemainingSubscriptionReadsHeaderKey))

	return remainingReads
}

// getRemainingSubscriptionWrites checking header for ARM level write count remaining
// this is used to be able to handle client reset if we've hit our limit
func getRemainingSubscriptionWrites(resp *http.Response) int {
	if resp == nil || resp.Header == nil || resp.Header.Get(consts.RemainingSubscriptionWritesHeaderKey) == "" {
		// 1200 because this is the default upper limit for subscription writes in ARM
		return 1200
	}

	remainingWrites, _ := strconv.Atoi(resp.Header.Get(consts.RemainingSubscriptionWritesHeaderKey))

	return remainingWrites
}

// getRemainingSubscriptionDeletes checking header for ARM level delete count remaining
// this is used to be able to handle client reset if we've hit our limit
func getRemainingSubscriptionDeletes(resp *http.Response) int {
	if resp == nil || resp.Header == nil || resp.Header.Get(consts.RemainingSubscriptionDeletesHeaderKey) == "" {
		// 15000 because this is the default upper limit for subscription deletes in ARM
		return 15000
	}

	remainingDeletes, _ := strconv.Atoi(resp.Header.Get(consts.RemainingSubscriptionDeletesHeaderKey))

	return remainingDeletes
}

// RecreateClientDueToArmLimits bool if we should reset the client to avoid ARM throttling
// ARM retruns a remaining limit we don't need read/write specific checks and limits are subscription wide
// regardless of what the intent of the call is
func RecreateClientDueToArmLimits(resp *http.Response) bool {
	// default to a limit that won't indicate we should reset the client
	remainingLimit := 2

	switch resp.Request.Method {
	case http.MethodGet:
		remainingLimit = getRemainingSubscriptionReads(resp)
	case http.MethodPut:
		remainingLimit = getRemainingSubscriptionWrites(resp)
	case http.MethodPatch:
		remainingLimit = getRemainingSubscriptionWrites(resp)
	case http.MethodDelete:
		remainingLimit = getRemainingSubscriptionDeletes(resp)
	}

	// true just before we've hit the request limits of ARM
	return remainingLimit <= 1

}
