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

package fnutil

type Set[T comparable] map[T]struct{}

func SliceToSet[T comparable](xs []T) Set[T] {
	rv := make(Set[T], len(xs))
	for _, x := range xs {
		rv[x] = struct{}{}
	}
	return rv
}

func SetToSlice[T comparable](s Set[T]) []T {
	rv := make([]T, 0, len(s))
	for x := range s {
		rv = append(rv, x)
	}
	return rv
}
