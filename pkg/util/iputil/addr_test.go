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

package iputil

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGroupAddressesByFamily(t *testing.T) {
	type args struct {
		vs []netip.Addr
	}
	tests := []struct {
		name string
		args args
		ipv4 []netip.Addr
		ipv6 []netip.Addr
	}{
		{
			name: "empty",
			args: args{
				vs: []netip.Addr{},
			},
			ipv4: nil,
			ipv6: nil,
		},
		{
			name: "1 IPv4",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
				},
			},
			ipv4: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
			},
			ipv6: nil,
		},
		{
			name: "N IPv4",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
					netip.MustParseAddr("192.168.0.1"),
					netip.MustParseAddr("172.10.0.0"),
				},
			},
			ipv4: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("172.10.0.0"),
			},
			ipv6: nil,
		},
		{
			name: "1 IPv6",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				},
			},
			ipv4: nil,
			ipv6: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
			},
		}, {
			name: "N IPv6",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
					netip.MustParseAddr("fd00::8a2e:370:7334"),
					netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
				},
			},
			ipv4: nil,
			ipv6: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				netip.MustParseAddr("fd00::8a2e:370:7334"),
				netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
			},
		}, {
			name: "1 IPv4 and 1 IPv6",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				},
			},
			ipv4: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
			},
			ipv6: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
			},
		}, {
			name: "N IPv4 and N IPv6",
			args: args{
				vs: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
					netip.MustParseAddr("192.168.0.1"),
					netip.MustParseAddr("172.16.0.0"),
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
					netip.MustParseAddr("fd00::8a2e:370:7334"),
					netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
				},
			},
			ipv4: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("172.16.0.0"),
			},
			ipv6: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				netip.MustParseAddr("fd00::8a2e:370:7334"),
				netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipv4, ipv6 := GroupAddressesByFamily(tt.args.vs)
			assert.Equalf(t, tt.ipv4, ipv4, "GroupAddressesByFamily(%v)", tt.args.vs)
			assert.Equalf(t, tt.ipv6, ipv6, "GroupAddressesByFamily(%v)", tt.args.vs)
		})
	}
}

func TestParseAddresses(t *testing.T) {
	type args struct {
		vs []string
	}
	tests := []struct {
		name    string
		args    args
		want    []netip.Addr
		wantErr bool
	}{
		{
			name: "empty",
			args: args{
				vs: []string{},
			},
			want:    nil,
			wantErr: false,
		},
		{
			name: "1 IPv4",
			args: args{
				vs: []string{
					"10.0.0.1",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
			},
			wantErr: false,
		},
		{
			name: "N IPv4",
			args: args{
				vs: []string{
					"10.0.0.1",
					"192.168.0.1",
					"172.10.0.0",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("172.10.0.0"),
			},
			wantErr: false,
		},
		{
			name: "1 IPv6",
			args: args{
				vs: []string{
					"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
			},
			wantErr: false,
		},
		{
			name: "N IPv6",
			args: args{
				vs: []string{
					"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
					"fd00::8a2e:370:7334",
					"fe80::200:5aee:feaa:20a2",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				netip.MustParseAddr("fd00::8a2e:370:7334"),
				netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
			},
			wantErr: false,
		},
		{
			name: "1 IPv4 and 1 IPv6",
			args: args{
				vs: []string{
					"10.0.0.1",
					"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
			},
			wantErr: false,
		},
		{
			name: "N IPv4 and N IPv6",
			args: args{
				vs: []string{
					"10.0.0.1",
					"192.168.0.1",
					"172.10.0.0",
					"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
					"fd00::8a2e:370:7334",
					"fe80::200:5aee:feaa:20a2",
				},
			},
			want: []netip.Addr{
				netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("172.10.0.0"),
				netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				netip.MustParseAddr("fd00::8a2e:370:7334"),
				netip.MustParseAddr("fe80::200:5aee:feaa:20a2"),
			},
			wantErr: false,
		},
		{
			name: "1 invalid",
			args: args{
				vs: []string{
					"foo",
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "1 invalid in IPv4",
			args: args{
				vs: []string{
					"10.0.0.1",
					"foo",
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "1 invalid in IPv6",
			args: args{
				vs: []string{
					"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
					"foo",
				},
			},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAddresses(tt.args.vs)
			if tt.wantErr {
				assert.Error(t, err, fmt.Sprintf("ParseAddresses(%v)", tt.args.vs))
				return
			}

			assert.Equalf(t, tt.want, got, "ParseAddresses(%v)", tt.args.vs)
		})
	}
}
