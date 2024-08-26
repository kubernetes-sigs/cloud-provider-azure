/*
Copyright 2024 The Kubernetes Authors.

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

package trace

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReconcile(t *testing.T) {
	t.Run("noop observe nil", func(t *testing.T) {
		ctx := context.Background()
		ctx, span := BeginReconcile(ctx, DefaultTracer(), "noop")
		assert.NotPanics(t, func() {
			span.Observe(ctx, nil)
		})
	})

	t.Run("noop observe error", func(t *testing.T) {
		ctx := context.Background()
		ctx, span := BeginReconcile(ctx, DefaultTracer(), "noop")
		assert.NotPanics(t, func() {
			span.Observe(ctx, fmt.Errorf("error"))
		})
	})

	t.Run("noop done", func(t *testing.T) {
		ctx := context.Background()
		ctx, span := BeginReconcile(ctx, DefaultTracer(), "noop")
		assert.NotPanics(t, func() {
			span.Done(ctx)
		})
	})

	t.Run("noop errored", func(t *testing.T) {
		ctx := context.Background()
		ctx, span := BeginReconcile(ctx, DefaultTracer(), "noop")
		assert.NotPanics(t, func() {
			span.Errored(ctx, fmt.Errorf("error"))
		})
	})
}
