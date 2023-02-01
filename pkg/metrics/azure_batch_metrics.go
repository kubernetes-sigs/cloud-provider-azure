/*
Copyright 2021 The Kubernetes Authors.

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
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/batch"
)

type BatchMetricsRecorder struct {
	attributes []string
}

type BatchProcessorMetricsRecorder struct {
	request string
	source  string
}

const keyAttributesSeparator = "|"

var (
	_ batch.MetricsRecorder          = &BatchMetricsRecorder{}
	_ batch.ProcessorMetricsRecorder = &BatchProcessorMetricsRecorder{}
)

// KeyFromAttributes concatenates the parameters to form a unique key for the referenced resource. Since the
// values are case-insensitive, they are converted to lowercase before concatenation.
func KeyFromAttributes(subscriptionID, resourceGroup, resourceName string) string {
	return strings.Join(
		[]string{
			strings.ToLower(subscriptionID),
			strings.ToLower(resourceGroup),
			strings.ToLower(resourceName)},
		keyAttributesSeparator,
	)
}

// AttributesFromKey extracts the attributes from a unique key created by KeyFromAttributes.
func AttributesFromKey(key string) (subscriptionID, resourceGroup, resourceName string) {
	attrs := strings.Split(key, keyAttributesSeparator)
	if len(attrs) != 3 {
		panic(fmt.Sprintf("invalid batch key: %q", key))
	}

	subscriptionID = attrs[0]
	resourceGroup = attrs[1]
	resourceName = attrs[2]
	return
}

// NewBatchProcessorMetricsRecorder creates a new batch processor metrics recorder.
func NewBatchProcessorMetricsRecorder(prefix, request, source string) *BatchProcessorMetricsRecorder {
	return &BatchProcessorMetricsRecorder{
		request: prefix + "_" + request,
		source:  source,
	}
}

// Records the processing start for a single batch of values for the specified key.
func (r *BatchProcessorMetricsRecorder) RecordBatchStart(key string) batch.MetricsRecorder {
	subscriptionID, resourceGroup, _ := AttributesFromKey(key)
	return &BatchMetricsRecorder{
		attributes: []string{r.request, resourceGroup, subscriptionID, r.source},
	}
}

// Records the rate limit delay, if any, incurred by the current batch.
func (r *BatchMetricsRecorder) RecordRateLimitDelay(delay time.Duration) {
	apiMetrics.rateLimitDelay.WithLabelValues(r.attributes...).Observe(delay.Seconds())
}

// Records completion of processing for the current batch.
func (r *BatchMetricsRecorder) RecordBatchCompletion(size int, err error) {
	apiMetrics.batchSize.WithLabelValues(r.attributes...).Observe(float64(size))
}
