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

package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/klog/v2"
)

var (
	defaultMetrics *Metrics
)

func init() {
	var err error
	defaultMetrics, err = New()
	if err != nil {
		klog.Fatalf("create default metrics: %v", err)
	}
}

func Default() *Metrics {
	return defaultMetrics
}

type Metrics struct {
	meter               metric.Meter
	apiLatency          metric.Float64Histogram
	apiErrorCount       metric.Int64Counter
	apiRateLimitedCount metric.Int64Counter
	apiThrottledCount   metric.Int64Counter
}

func New() (*Metrics, error) {
	meter := otel.Meter("cloud-provider-azclient")

	apiLatency, err := meter.Float64Histogram(
		"api_request_duration_seconds",
		metric.WithDescription("Latency of an Azure API call"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(.1, .25, .5, 1, 2.5, 5, 10, 15, 25, 50, 120, 300, 600, 1200),
	)
	if err != nil {
		return nil, fmt.Errorf("create api_request_duration_seconds histogram: %w", err)
	}

	apiErrorCount, err := meter.Int64Counter(
		"api_request_errors",
		metric.WithDescription("Number of errors for an Azure API call"),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create api_request_errors counter: %w", err)
	}

	apiRateLimitedCount, err := meter.Int64Counter(
		"api_request_ratelimited_count",
		metric.WithDescription("Number of rate limited Azure API calls"),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create api_request_ratelimited_count counter: %w", err)
	}

	apiThrottledCount, err := meter.Int64Counter(
		"api_request_throttled_count",
		metric.WithDescription("Number of throttled Azure API calls"),
		metric.WithUnit("{call}"),
	)
	return &Metrics{
		meter:               meter,
		apiLatency:          apiLatency,
		apiErrorCount:       apiErrorCount,
		apiRateLimitedCount: apiRateLimitedCount,
		apiThrottledCount:   apiThrottledCount,
	}, nil
}

type Span struct {
	metrics    *Metrics
	start      time.Time
	attributes []attribute.KeyValue
}

func (m *Metrics) NewSpan(prefix, requestMethod, resourceGroup, subscriptionID string) *Span {
	return &Span{
		metrics: m,
		start:   time.Now(),
		attributes: []attribute.KeyValue{
			attribute.String("request", fmt.Sprintf("%s_%s", prefix, requestMethod)),
			attribute.String("resource_group", resourceGroup),
			attribute.String("subscription_id", subscriptionID),
		},
	}
}

func (s *Span) RateLimited(ctx context.Context) {
	s.metrics.apiRateLimitedCount.Add(ctx, 1, metric.WithAttributes(s.attributes...))
}

func (s *Span) Throttled(ctx context.Context) {
	s.metrics.apiThrottledCount.Add(ctx, 1, metric.WithAttributes(s.attributes...))
}

func (s *Span) Observe(ctx context.Context, err error) {
	latency := time.Since(s.start).Seconds()
	s.metrics.apiLatency.Record(ctx, latency, metric.WithAttributes(s.attributes...))

	var respErr *azcore.ResponseError
	var errorCode string
	if errors.As(err, &respErr) {
		errorCode = respErr.ErrorCode
		attributes := append(s.attributes, attribute.String("error_code", errorCode))
		s.metrics.apiErrorCount.Add(ctx, 1, metric.WithAttributes(attributes...))
	}
}
