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

package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_bitAt(t *testing.T) {
	bytes := []byte{0b1010_1010, 0b0101_0101}
	assert.Equal(t, uint8(1), bitAt(bytes, 0))
	assert.Equal(t, uint8(0), bitAt(bytes, 1))
	assert.Equal(t, uint8(1), bitAt(bytes, 2))
	assert.Equal(t, uint8(0), bitAt(bytes, 3))

	assert.Equal(t, uint8(1), bitAt(bytes, 4))
	assert.Equal(t, uint8(0), bitAt(bytes, 5))
	assert.Equal(t, uint8(1), bitAt(bytes, 6))
	assert.Equal(t, uint8(0), bitAt(bytes, 7))

	assert.Equal(t, uint8(0), bitAt(bytes, 8))
	assert.Equal(t, uint8(1), bitAt(bytes, 9))
	assert.Equal(t, uint8(0), bitAt(bytes, 10))
	assert.Equal(t, uint8(1), bitAt(bytes, 11))

	assert.Equal(t, uint8(0), bitAt(bytes, 12))
	assert.Equal(t, uint8(1), bitAt(bytes, 13))
	assert.Equal(t, uint8(0), bitAt(bytes, 14))
	assert.Equal(t, uint8(1), bitAt(bytes, 15))

	assert.Panics(t, func() { bitAt(bytes, 16) })
}

func Test_setBitAt(t *testing.T) {
	tests := []struct {
		name     string
		initial  []byte
		index    int
		bit      uint8
		expected []byte
	}{
		{
			name:     "Set first bit to 1",
			initial:  []byte{0b0000_0000},
			index:    0,
			bit:      1,
			expected: []byte{0b1000_0000},
		},
		{
			name:     "Set last bit to 1",
			initial:  []byte{0b0000_0000},
			index:    7,
			bit:      1,
			expected: []byte{0b0000_0001},
		},
		{
			name:     "Set middle bit to 1",
			initial:  []byte{0b0000_0000},
			index:    4,
			bit:      1,
			expected: []byte{0b0000_1000},
		},
		{
			name:     "Set bit to 0",
			initial:  []byte{0b1111_1111},
			index:    3,
			bit:      0,
			expected: []byte{0b1110_1111},
		},
		{
			name:     "Set bit in second byte",
			initial:  []byte{0b0000_0000, 0b0000_0000},
			index:    9,
			bit:      1,
			expected: []byte{0b0000_0000, 0b0100_0000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBitAt(tt.initial, tt.index, tt.bit)
			assert.Equal(t, tt.expected, tt.initial)
		})
	}

	assert.Panics(t, func() { setBitAt([]byte{0x00}, 8, 1) })
}
