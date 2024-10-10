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

package errutils

import (
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func CheckResourceExistsFromAzcoreError(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var respError *azcore.ResponseError
	if errors.As(err, &respError) && respError != nil {
		if respError.StatusCode == http.StatusNotFound {
			return false, nil
		}
	}
	return false, err
}
