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

package sets

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestNewString(t *testing.T) {
	tests := []struct {
		name          string
		items         []string
		expectedItems sets.Set[string]
	}{
		{
			name:  "empty",
			items: nil,
		},
		{
			name:          "non-empty",
			items:         []string{"Foo", "Bar"},
			expectedItems: sets.New[string]("foo", "bar"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewString(tt.items...)
			if !s.set.Equal(tt.expectedItems) {
				t.Errorf("NewString() = %v, want %v", s.set, tt.expectedItems)
			}
		})
	}
}

func TestInsert(t *testing.T) {
	tests := []struct {
		name          string
		set           *IgnoreCaseSet
		items         []string
		expectedItems sets.Set[string]
	}{
		{
			name:          "empty set",
			set:           NewString(),
			items:         []string{"foo"},
			expectedItems: sets.New[string]("foo"),
		},
		{
			name:          "non-empty set",
			set:           NewString("foo"),
			items:         []string{"bar"},
			expectedItems: sets.New[string]("foo", "bar"),
		},
		{
			name:          "non-empty set with existing items",
			set:           NewString("foo"),
			items:         []string{"foo", "bar"},
			expectedItems: sets.New[string]("foo", "bar"),
		},
		{
			name:          "non-empty set with different case",
			set:           NewString("foo"),
			items:         []string{"FOO"},
			expectedItems: sets.New[string]("foo"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.set.Insert(tt.items...)
			if !tt.set.set.Equal(tt.expectedItems) {
				t.Errorf("Insert() = %v, want %v", tt.set.set, tt.expectedItems)
			}
		})
	}
}

func TestSafeInsert(t *testing.T) {
	tests := []struct {
		name          string
		set           *IgnoreCaseSet
		items         []string
		expectedItems sets.Set[string]
	}{
		{
			name:          "nil set",
			set:           nil,
			items:         []string{"Foo"},
			expectedItems: sets.New[string]("foo"),
		},
		{
			name:          "empty set",
			set:           NewString(),
			items:         []string{"foo"},
			expectedItems: sets.New[string]("foo"),
		},
		{
			name:          "non-empty set",
			set:           NewString("foo"),
			items:         []string{"bar"},
			expectedItems: sets.New[string]("foo", "bar"),
		},
		{
			name:          "non-empty set with existing items",
			set:           NewString("foo"),
			items:         []string{"foo", "bar"},
			expectedItems: sets.New[string]("foo", "bar"),
		},
		{
			name:          "non-empty set with different case",
			set:           NewString("foo"),
			items:         []string{"FOO"},
			expectedItems: sets.New[string]("foo"),
		},
		{
			name:          "empty set with nil inner set",
			set:           &IgnoreCaseSet{},
			items:         []string{"foo"},
			expectedItems: sets.New[string]("foo"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := SafeInsert(tt.set, tt.items...)
			if !s.set.Equal(tt.expectedItems) {
				t.Errorf("SafeInsert() = %v, want %v", s.set, tt.expectedItems)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	tests := []struct {
		name          string
		set           *IgnoreCaseSet
		item          string
		expectedItems sets.Set[string]
		want          bool
	}{
		{
			name:          "nil set",
			set:           nil,
			item:          "foo",
			expectedItems: nil,
			want:          false,
		},
		{
			name:          "empty set",
			set:           NewString(),
			item:          "foo",
			expectedItems: nil,
			want:          false,
		},
		{
			name:          "non-empty set",
			set:           NewString("foo"),
			item:          "foo",
			expectedItems: nil,
			want:          true,
		},
		{
			name:          "non-empty set with different case",
			set:           NewString("foo"),
			item:          "FOO",
			expectedItems: nil,
			want:          true,
		},
		{
			name:          "non-empty set with different item",
			set:           NewString("foo"),
			item:          "bar",
			expectedItems: sets.New[string]("foo"),
			want:          false,
		},
		{
			name:          "empty set with nil inner set",
			set:           &IgnoreCaseSet{},
			item:          "foo",
			expectedItems: nil,
			want:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Delete(tt.item); got != tt.want {
				t.Errorf("Delete() = %v, want %v", got, tt.want)
			}
			if tt.expectedItems != nil && !tt.set.set.Equal(tt.expectedItems) {
				t.Errorf("Delete() = %v, want %v", tt.set.set, tt.expectedItems)
			}
		})
	}
}

func TestIsInitialized(t *testing.T) {
	tests := []struct {
		name string
		set  *IgnoreCaseSet
		want bool
	}{
		{
			name: "nil set",
			set:  nil,
			want: false,
		},
		{
			name: "empty set",
			set:  NewString(),
			want: true,
		},
		{
			name: "non-empty set",
			set:  NewString("foo"),
			want: true,
		},
		{
			name: "empty set with nil inner set",
			set:  &IgnoreCaseSet{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Initialized(); got != tt.want {
				t.Errorf("Initialized() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHas(t *testing.T) {
	tests := []struct {
		name string
		set  *IgnoreCaseSet
		item string
		want bool
	}{
		{
			name: "nil set",
			set:  nil,
			item: "foo",
			want: false,
		},
		{
			name: "empty set",
			set:  NewString(),
			item: "foo",
			want: false,
		},
		{
			name: "non-empty set",
			set:  NewString("foo"),
			item: "foo",
			want: true,
		},
		{
			name: "non-empty set with different case",
			set:  NewString("foo"),
			item: "FOO",
			want: true,
		},
		{
			name: "non-empty set with different item",
			set:  NewString("foo"),
			item: "bar",
			want: false,
		},
		{
			name: "empty set with nil inner set",
			set:  &IgnoreCaseSet{},
			item: "foo",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Has(tt.item); got != tt.want {
				t.Errorf("Has() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnsortedList(t *testing.T) {
	tests := []struct {
		name string
		set  *IgnoreCaseSet
		want []string
	}{
		{
			name: "nil set",
			set:  nil,
			want: nil,
		},
		{
			name: "empty set",
			set:  NewString(),
			want: []string{},
		},
		{
			name: "non-empty set",
			set:  NewString("foo", "bar"),
			want: []string{"bar", "foo"},
		},
		{
			name: "empty set with nil inner set",
			set:  &IgnoreCaseSet{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.UnsortedList(); !sets.New[string](got...).Equal(sets.New[string](tt.want...)) {
				t.Errorf("UnsortedList() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLen(t *testing.T) {
	tests := []struct {
		name string
		set  *IgnoreCaseSet
		want int
	}{
		{
			name: "nil set",
			set:  nil,
			want: 0,
		},
		{
			name: "empty set",
			set:  NewString(),
			want: 0,
		},
		{
			name: "non-empty set",
			set:  NewString("foo", "bar"),
			want: 2,
		},
		{
			name: "empty set with nil inner set",
			set:  &IgnoreCaseSet{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Len(); got != tt.want {
				t.Errorf("Len() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEquals(t *testing.T) {
	tests := []struct {
		name string
		s1   *IgnoreCaseSet
		s2   *IgnoreCaseSet
		want bool
	}{
		{
			name: "both nil",
			s1:   nil,
			s2:   nil,
			want: true,
		},
		{
			name: "first nil",
			s1:   nil,
			s2:   NewString("foo"),
			want: false,
		},
		{
			name: "second nil",
			s1:   NewString("foo"),
			s2:   nil,
			want: false,
		},
		{
			name: "empty sets",
			s1:   NewString(),
			s2:   NewString(),
			want: true,
		},
		{
			name: "same elements",
			s1:   NewString("foo", "bar"),
			s2:   NewString("foo", "bar"),
			want: true,
		},
		{
			name: "same elements with different case",
			s1:   NewString("foo", "bar"),
			s2:   NewString("FOO", "BAR"),
			want: true,
		},
		{
			name: "different sizes",
			s1:   NewString("foo", "bar"),
			s2:   NewString("foo"),
			want: false,
		},
		{
			name: "same size but different elements",
			s1:   NewString("foo", "bar"),
			s2:   NewString("foo", "baz"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.s1 == nil && tt.s2 == nil {
				// Special case for nil sets
				if !tt.want {
					t.Errorf("Equals() = true, want %v", tt.want)
				}
				return
			}
			if tt.s1 == nil || tt.s2 == nil {
				// One set is nil, they can't be equal
				if tt.want {
					t.Errorf("Equals() = false, want %v", tt.want)
				}
				return
			}
			if got := tt.s1.Equals(tt.s2); got != tt.want {
				t.Errorf("Equals() = %v, want %v", got, tt.want)
			}
		})
	}
}
