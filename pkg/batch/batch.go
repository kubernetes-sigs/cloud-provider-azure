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
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

const (
	defaultVerboseLogLevel int = 1
)

type EntryValue interface {
	CleanUp()
}

// newEntry returns a new batch entry.
func newEntry(ctx context.Context, value interface{}) *entry {
	return &entry{ctx: ctx, value: value, resultChan: make(chan Result, 1)}
}

// setResult sets the result of a batch entry.
func (e *entry) setResult(value interface{}, err error) {
	if entryValue, ok := e.value.(EntryValue); ok {
		entryValue.CleanUp()
	}
	e.resultChan <- Result{value: value, err: err}
	close(e.resultChan)
}

// getResult waits for and returns the result of a batch entry.
func (e *entry) getResult(ctx context.Context) (interface{}, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case r := <-e.resultChan:
		return r.value, r.err
	}
}

// getResultChar returns the result channel of a batch entry.
func (e *entry) getResultChan() <-chan (Result) {
	return e.resultChan
}

// add adds a value to the batch. It the newly added entry and a Boolean value that contains true if
// the batch was empty and the returned entry was the first to be added.
func (b *batcher) add(ctx context.Context, value interface{}) (e *entry, first bool) {
	e = newEntry(ctx, value)

	b.mutex.Lock()
	first = len(b.entries) == 0
	b.entries = append(b.entries, e)
	b.mutex.Unlock()

	return
}

// isEmpty returns true if the batch is currently empty.
func (b *batcher) isEmpty() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return len(b.entries) == 0
}

// flush returns the current batch of values and empties the batch. It also returns the time of the
// furthest context deadline in the batch. If no entries specify a deadline in their context,
// deadline.IsZero() == true.
func (b *batcher) flush() (flushed []*entry, nextDeadline time.Time) {
	var entries []*entry

	b.mutex.Lock()
	entries = b.entries
	b.entries = make([]*entry, 0)
	b.mutex.Unlock()

	for _, entry := range entries {
		if ctxErr := entry.ctx.Err(); ctxErr != nil {
			entry.setResult(nil, ctxErr)
		} else {
			flushed = append(flushed, entry)

			if deadline, ok := entry.ctx.Deadline(); ok && deadline.After(nextDeadline) {
				nextDeadline = deadline
			}
		}
	}

	return
}

// purgeExpiredOrCanceled removes any expired or canceled entries from the batch. It returns a Boolean
// indicating whether the batch contains unexpired or uncanceled entries and a time value representing
// the furthest context deadline in the remaining batch (deadline.IsZero() == true if no contexts
// specify a deadline). If the batch is emptied by this function, it returns false and a time value of
// zero.
func (b *batcher) purgeExpiredOrCanceled() (more bool, nextDeadline time.Time) {
	kept := make([]*entry, 0)

	b.mutex.Lock()
	for _, entry := range b.entries {
		if ctxErr := entry.ctx.Err(); ctxErr != nil {
			entry.setResult(nil, ctxErr)
		} else {
			kept = append(kept, entry)

			if deadline, ok := entry.ctx.Deadline(); ok && deadline.After(nextDeadline) {
				nextDeadline = deadline
			}
		}
	}

	more = len(kept) > 0
	b.entries = kept
	b.mutex.Unlock()

	return
}

// tryEnterProcessLoop returns true if the batch processing function can be invoked by this goroutine.
// This function ensures that only one goroutine at any given time can enter the batch processing loop.
//
// The returned function must be called on exit from the processing loop.
func (b *batcher) tryEnterProcessLoop() (bool, func()) {
	deadline := time.Now().Add(2 * time.Minute)

	for {
		acquiredSem := func() bool {
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			err := b.sem.Acquire(ctx, 1)
			if err == nil {
				return true
			}

			var more bool
			more, deadline = b.purgeExpiredOrCanceled()
			if more && deadline.IsZero() {
				deadline = time.Now().Add(2 * time.Minute)
			}

			return false
		}()

		if acquiredSem {
			if b.deleted {
				b.sem.Release(1)
				return false, nil
			}

			return true, func() { b.sem.Release(1) }
		}

		if deadline.IsZero() {
			return false, nil
		}
	}
}

// do adds the value to the batch and invokes the batch processing function. It guarantees that only one
// invocation may be in-flight at any given time.
func (b *batcher) do(
	ctx context.Context,
	processor *Processor,
	key string,
	value interface{},
) *entry {

	thisEntry, first := b.add(ctx, value)

	if first {
		go func() {
			entered, exitLoop := b.tryEnterProcessLoop()
			if !entered {
				return
			}
			defer exitLoop()

			delayBeforeStart := processor.delayBeforeStart
			limiter := processor.getLimiterFn(key)

			for !b.deleted && !b.isEmpty() {
				// Scope the loop deferrals in a function closure.
				func() {
					var (
						batchSize int
						err       error
					)

					batchRecorder := processor.metricsRecorder.RecordBatchStart(key)
					defer func() { batchRecorder.RecordBatchCompletion(batchSize, err) }()

					// Delay start of batch processing based on the max of the initial delay and delay due to throttling.
					reservation := limiter.Reserve()
					timeToDelay := maxDuration(delayBeforeStart, reservation.Delay())

					if timeToDelay > 0 {
						time.Sleep(timeToDelay)

						if timeToDelay == delayBeforeStart {
							processor.logger.V(processor.verboseLogLevel).Info("Delayed processing of batch due to start delay", "key", key, "delay", timeToDelay)
						} else {
							processor.logger.Info("Delayed processing of batch due to client-side throttling", "key", key, "delay", timeToDelay)
							batchRecorder.RecordRateLimitDelay(timeToDelay)
						}
					}

					// Get the next batch of entries to process.
					entries, deadline := b.flush()
					batchSize = len(entries)
					if batchSize <= 0 {
						return
					}

					values := make([]interface{}, len(entries))
					for i, entry := range entries {
						values[i] = entry.value
					}

					innerCtx := context.Background()
					if !deadline.IsZero() {
						var cancel func()
						innerCtx, cancel = context.WithDeadline(innerCtx, deadline)
						defer cancel()
					}

					var results []interface{}
					results, err = processor.fn(innerCtx, key, values)

					for i, entry := range entries {
						var valResult interface{}
						errResult := err

						numResults := len(results)
						if i < numResults {
							if errTemp, ok := results[i].(error); !ok {
								valResult = results[i]
							} else {
								errResult = errTemp
							}
						} else if numResults != 0 {
							errResult = fmt.Errorf("result not received for entry (%v)", entry.value)
							processor.logger.Error(errResult, "Missing result")
						}
						entry.setResult(valResult, errResult)
					}
				}()

				// We only want to delay before the initial start of batch processing assuming that more
				// requests will come in before the configured delay. On subsequent iterations of this loop,
				// we rely on the latency of the operation to naturally batch new requests.
				delayBeforeStart = 0
			}
		}()
	}

	return thisEntry
}

// getBatcher returns the batcher for the specified key.
func (p *Processor) getBatcher(key string) *batcher {
	v, _ := p.batches.LoadOrStore(key, &batcher{sem: semaphore.NewWeighted(1)})
	return v.(*batcher)
}

// Do adds the value to the keyed batch and invokes the batch processing function. It guarantees that only one
// invocation may be in-flight at any given time for the specified key.
func (p *Processor) Do(ctx context.Context, key string, value interface{}) (interface{}, error) {
	batcher := p.getBatcher(key)
	entry := batcher.do(ctx, p, key, value)

	return entry.getResult(ctx)
}

// DoChan performs the same action as Do, but returns a channel that the caller uses to receive results
// asynchronously.
func (p *Processor) DoChan(ctx context.Context, key string, value interface{}) <-chan (Result) {
	batcher := p.getBatcher(key)
	entry := batcher.do(ctx, p, key, value)

	return entry.getResultChan()
}

// Deletes the keyed batch. The function returns when all batch processing has stopped for the specified key.
// Operations currently in-flight will complete, but any pending requests will be canceled.
func (p *Processor) Delete(key string) {
	if v, ok := p.batches.LoadAndDelete(key); ok {
		b := v.(*batcher)
		_ = b.sem.Acquire(context.Background(), 1)
		defer b.sem.Release(1)
		b.deleted = true

		pending, _ := b.flush()
		for _, entry := range pending {
			entry.setResult(nil, context.Canceled)
		}

		p.limiters.Delete(key)
	}
}

// applyDefaults applies default options to the Processor.
func (p *Processor) applyDefaults() {
	if p.getLimiterFn == nil {
		var limiterMap sync.Map

		p.getLimiterFn = func(key string) *rate.Limiter {
			if v, ok := limiterMap.Load(key); ok {
				return v.(*rate.Limiter)
			}

			limiter := rate.NewLimiter(rate.Inf, 0)

			v, _ := limiterMap.LoadOrStore(key, limiter)

			return v.(*rate.Limiter)
		}
	}

	if p.logger.GetSink() == nil {
		p.logger = logr.Discard()
	}

	if p.verboseLogLevel == -1 {
		p.verboseLogLevel = defaultVerboseLogLevel
	}

	if p.metricsRecorder == nil {
		p.metricsRecorder = &nullRecorder
	}
}

// NewProcessor returns a new batch processor.
func NewProcessor(fn ProcessFunc, options ...ProcessorOption) *Processor {
	p := &Processor{fn: fn, verboseLogLevel: -1}

	for _, option := range options {
		option(p)
	}

	p.applyDefaults()

	return p
}

// WithDelayBeforeStart applies the specified time delay before starting batch processing for each unique key.
func WithDelayBeforeStart(delay time.Duration) ProcessorOption {
	return func(p *Processor) {
		p.delayBeforeStart = delay
	}
}

// WithGlobalLimiter applies a rate limiter that is used to throttle batch processing of all keys.
func WithGlobalLimiter(limiter *rate.Limiter) ProcessorOption {
	return func(p *Processor) {
		p.getLimiterFn = func(key string) *rate.Limiter {
			return limiter
		}
	}
}

// WithGlobalLimits applies rate limits that are used to throttle batch processing of all keys.
func WithGlobalLimits(limit rate.Limit, burst int) ProcessorOption {
	return WithGlobalLimiter(rate.NewLimiter(limit, burst))
}

// WithBatchLimits applies rate limits that are used to throttle batch processing of each key independently.
func WithBatchLimits(limit rate.Limit, burst int) ProcessorOption {
	return func(p *Processor) {
		p.getLimiterFn = func(key string) *rate.Limiter {
			if v, ok := p.limiters.Load(key); ok {
				return v.(*rate.Limiter)
			}

			batchLimiter := rate.NewLimiter(limit, burst)

			v, _ := p.limiters.LoadOrStore(key, batchLimiter)

			return v.(*rate.Limiter)
		}
	}
}

// WithLogger sets the logger.
func WithLogger(l logr.Logger) ProcessorOption {
	return func(p *Processor) {
		p.logger = l
	}
}

// WithVerboseLogLevel sets the verbose logging level
func WithVerboseLogLevel(level int) ProcessorOption {
	return func(p *Processor) {
		p.verboseLogLevel = level
	}
}

// WithMetricsRecorder sets the metrics recorder.
func WithMetricsRecorder(metricsRecorder ProcessorMetricsRecorder) ProcessorOption {
	return func(p *Processor) {
		p.metricsRecorder = metricsRecorder
	}
}
