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

package providererrors_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	providererrors "sigs.k8s.io/cloud-provider-azure/pkg/provider/errors"
)

func TestErrLoadBalancerMetadataUnavailable(t *testing.T) {
	cause := errors.New("connection reset")
	err := fmt.Errorf("%w: %w", providererrors.ErrLoadBalancerMetadataUnavailable, cause)

	assert.Equal(t, "load balancer metadata unavailable", providererrors.ErrLoadBalancerMetadataUnavailable.Error())
	assert.Equal(t, "load balancer metadata unavailable: connection reset", err.Error())
	assert.True(t, errors.Is(err, providererrors.ErrLoadBalancerMetadataUnavailable))
	assert.True(t, errors.Is(err, cause))
	assert.False(t, errors.Is(cause, providererrors.ErrLoadBalancerMetadataUnavailable))
}
