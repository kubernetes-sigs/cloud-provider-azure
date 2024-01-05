/*
Copyright 2023 The Kubernetes Authors.

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

package testutil

import (
	"encoding/json"
)

// CloneInJSON clones an object mapping in JSON.
// Normally, all ARM resources are JSON mapping.
func CloneInJSON[T any](v T) T {
	encoded, err := json.Marshal(v)
	if err != nil {
		panic("unreachable: it should be always ok to marshal the object")
	}
	var rv T
	err = json.Unmarshal(encoded, &rv)
	if err != nil {
		panic("unreachable: it should be always ok to unmarshal the object")
	}

	return rv
}
