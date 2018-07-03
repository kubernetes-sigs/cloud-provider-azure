/*
Copyright 2018 The Kubernetes Authors.

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

package validation

import (
	"sort"
)

func CheckTest(skips []Description, reports []Description) bool {
	for index := range skips {
		sort.Slice(skips[index].Subtest, func(i, j int) bool { return skips[index].Subtest[i] < skips[index].Subtest[j] })
		sort.Slice(reports[index].Subtest, func(i, j int) bool { return reports[index].Subtest[i] < reports[index].Subtest[j] })
	}
	for i := range skips {
		if len(skips[i].Subtest) != len(reports[i].Subtest) {
			return false
		}
		for j := range skips[i].Subtest {
			if skips[i].Subtest[j] != reports[i].Subtest[j] {
				return false
			}
		}
	}
	return true
}
