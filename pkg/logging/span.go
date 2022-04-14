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

package logging

import (
	"runtime"
	"time"

	"k8s.io/klog/v2"
)

const VerbosityForSpan klog.Level = 5

// Span is a helper for logging the start point and the end point of the process block.
type Span interface {

	// Start prints the log indicating the process block has started.
	// It should be called only once.
	Start() Span

	// Finish prints the log indicating the process block has reached the end.
	// And it should be called only once too.
	Finish(err error, extraFields ...interface{})
}

var (
	_ Span = &span{}
	_ Span = &noOpSpan{}
)

type span struct {
	name   string
	fields []interface{}
	now    time.Time
}

// SpanForFunc is a helper to create `Span` block with caller's name.
// If the level of verbosity is less than *5*, it returns the `no-op` implementation.
func SpanForFunc(fields ...interface{}) Span {
	if !klog.V(VerbosityForSpan).Enabled() {
		return &noOpSpan{}
	}

	name := "unknown"
	if pc, _, _, ok := runtime.Caller(1); ok {
		name = runtime.FuncForPC(pc).Name()
	}

	return SpanForBlock(name, fields...)
}

// SpanForBlock returns an `Span` for logging block.
// The given name will be logged as logr's key-value pair with `name` as the key.
// If the level of verbosity is less than *5*, it returns the `no-op` implementation.
func SpanForBlock(name string, fields ...interface{}) Span {
	if !klog.V(VerbosityForSpan).Enabled() {
		return &noOpSpan{}
	}

	return &span{
		name:   name,
		fields: fields,
		now:    time.Now(),
	}
}

func (l *span) Start() Span {
	klog.InfoSDepth(1, "process started", append(l.fields, "name", l.name)...)
	return l
}

func (l *span) Finish(err error, extraFields ...interface{}) {
	fields := append(l.fields, extraFields...)
	fields = append(fields,
		"name", l.name,
		"error", err,
		"took-ms", time.Since(l.now).Milliseconds())

	klog.InfoSDepth(1, "process finished", fields...)
}

type noOpSpan struct{}

func (e *noOpSpan) Start() Span                      { return e }
func (e *noOpSpan) Finish(_ error, _ ...interface{}) {}
