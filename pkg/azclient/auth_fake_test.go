/*
Copyright 2025 The Kubernetes Authors.

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

package azclient

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/assert"
)

var (
	incCounter = atomic.Int64{}
)

type fakeTokenCredential struct {
	ID string
}

func newFakeTokenCredential() *fakeTokenCredential {
	id := fmt.Sprintf("fake-token-credential-%d-%d", incCounter.Add(1), time.Now().UnixNano())
	return &fakeTokenCredential{ID: id}
}

func (f *fakeTokenCredential) GetToken(
	_ context.Context,
	_ policy.TokenRequestOptions,
) (azcore.AccessToken, error) {
	panic("not implemented")
}

type AuthProviderAssertions func(t testing.TB, authProvider *AuthProvider)

func ApplyAssertions(t testing.TB, authProvider *AuthProvider, assertions []AuthProviderAssertions) {
	t.Helper()

	for _, assertion := range assertions {
		assertion(t, authProvider)
	}
}

func AssertComputeTokenCredential(tokenCredential *fakeTokenCredential) AuthProviderAssertions {
	return func(t testing.TB, authProvider *AuthProvider) {
		t.Helper()

		assert.NotNil(t, authProvider.ComputeCredential)

		cred, ok := authProvider.ComputeCredential.(*fakeTokenCredential)
		assert.True(t, ok, "expected a fake token credential")
		assert.Equal(t, tokenCredential.ID, cred.ID)
	}
}

func AssertNetworkTokenCredential(tokenCredential *fakeTokenCredential) AuthProviderAssertions {
	return func(t testing.TB, authProvider *AuthProvider) {
		t.Helper()

		assert.NotNil(t, authProvider.NetworkCredential)

		cred, ok := authProvider.NetworkCredential.(*fakeTokenCredential)
		assert.True(t, ok, "expected a fake token credential")
		assert.Equal(t, tokenCredential.ID, cred.ID)
	}
}

func AssertNilNetworkTokenCredential() AuthProviderAssertions {
	return func(t testing.TB, authProvider *AuthProvider) {
		t.Helper()

		assert.Nil(t, authProvider.NetworkCredential)
	}
}

func AssertEmptyAdditionalComputeClientOptions() AuthProviderAssertions {
	return func(t testing.TB, authProvider *AuthProvider) {
		t.Helper()

		assert.Empty(t, authProvider.AdditionalComputeClientOptions)
	}
}

func AssertCloudConfig(expected cloud.Configuration) AuthProviderAssertions {
	return func(t testing.TB, authProvider *AuthProvider) {
		t.Helper()

		assert.Equal(t, expected, authProvider.CloudConfig)
	}
}
