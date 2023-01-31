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

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// Result represents the Result of a batched call for a specific value.
type Result struct {
	value interface{}
	err   error
}

// entry provides facilities to asynchronously wait for results of a batched call for a specific value.
type entry struct {
	ctx        context.Context
	value      interface{}
	resultChan chan (Result)
}

// batcher provides facilities to batch values and invoke a function to process a batch of values with
// the guarantee that only a single invocation can be in-flight at any given time.
type batcher struct {
	mutex   sync.Mutex
	entries []*entry
	sem     *semaphore.Weighted
	deleted bool
}

// ProcessFunc is the function that processes a batch of values and return results.
//
// An implementation receives an slice of values to process. It must return an slice of results of the
// same size; each element corresponding to a result for the element at the same index in the values
// slice. If an entry contains an error, an error will be returned to the caller that submitted the
// value. The batch function may return an empty or nil slice iff it returns an error.
type ProcessFunc func(ctx context.Context, key string, values []interface{}) ([]interface{}, error)

// GetLimiterFunc returns the limiter for a specific batch key. The function must return the same
// value for any given key.
type GetLimiterFunc func(key string) *rate.Limiter

// GetProcessorMetricsRecorderFunc returns a metrics recorder a Processor can use to record metrics.
type GetProcessorMetricsRecorderFunc func() ProcessorMetricsRecorder

// ProcessorOption modifies the Process configuration.
type ProcessorOption func(p *Processor)

// Processor provides facilities to batch values in keyed batches and invoke a function to process a
// batch of values. It guarantees that only a single invocation of the function can be in-flight at any
// given time for each keyed batch.
type Processor struct {
	fn               ProcessFunc
	delayBeforeStart time.Duration
	getLimiterFn     GetLimiterFunc
	logger           logr.Logger
	verboseLogLevel  int
	metricsRecorder  ProcessorMetricsRecorder
	batches          sync.Map
	limiters         sync.Map
}
