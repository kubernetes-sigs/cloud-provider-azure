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

package batch

import "time"

// MetricsRecorder defines an interface for recording batch metrics.
type MetricsRecorder interface {
	// Records the rate limit delay, if any, incurred by the current batch.
	RecordRateLimitDelay(delay time.Duration)

	// Records completion of processing for the current batch.
	RecordBatchCompletion(size int, err error)
}

// ProcessorMetricsRecorder defines an interface for recording batch processor metrics.
type ProcessorMetricsRecorder interface {
	// Records the processing start for a single batch of values for the specified key.
	RecordBatchStart(key string) MetricsRecorder
}
