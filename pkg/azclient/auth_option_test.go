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
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/msi-dataplane/pkg/dataplane"
	"github.com/stretchr/testify/assert"
)

func ExpectFuncEqual(t testing.TB, expected, actual any) {
	t.Helper()

	exp := runtime.FuncForPC(reflect.ValueOf(expected).Pointer()).Name()
	act := runtime.FuncForPC(reflect.ValueOf(actual).Pointer()).Name()
	assert.Equal(t, exp, act)
}

func TestDefaultAuthProviderOptions(t *testing.T) {
	t.Parallel()

	opts := defaultAuthProviderOptions()
	assert.NotNil(t, opts)
	assert.NotNil(t, opts.NewWorkloadIdentityCredentialFn)
	assert.NotNil(t, opts.NewManagedIdentityCredentialFn)
	assert.NotNil(t, opts.NewClientSecretCredentialFn)
	assert.NotNil(t, opts.NewClientCertificateCredentialFn)
	assert.NotNil(t, opts.NewKeyVaultCredentialFn)

	assert.NotNil(t, opts.NewUserAssignedIdentityCredentialFn)
	ExpectFuncEqual(t, dataplane.NewUserAssignedIdentityCredential, opts.NewUserAssignedIdentityCredentialFn)

	assert.NotNil(t, opts.ReadFileFn)
	ExpectFuncEqual(t, os.ReadFile, opts.ReadFileFn)

	assert.NotNil(t, opts.ParseCertificatesFn)
	ExpectFuncEqual(t, azidentity.ParseCertificates, opts.ParseCertificatesFn)
}
