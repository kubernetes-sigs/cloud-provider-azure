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

package stringutils

import (
	"testing"
)

func TestHasPrefixCaseInsensitive(t *testing.T) {
	cases := []struct {
		s      string
		prefix string
		want   bool
	}{
		{"HelloWorld", "hello", true},
		{"HelloWorld", "WORLD", false},
		{"", "prefix", false},
		{"CaseSensitive", "casesensitive", true},
		{"CaseSensitive", "CASE", true},
	}

	for _, c := range cases {
		got := HasPrefixCaseInsensitive(c.s, c.prefix)
		if got != c.want {
			t.Errorf("HasPrefixCaseInsensitive(%q, %q) == %v, want %v", c.s, c.prefix, got, c.want)
		}
	}
}
