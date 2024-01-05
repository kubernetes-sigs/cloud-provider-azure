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

package fixture

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"net/netip"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
)

type Fixture struct{}

func NewFixture() *Fixture {
	return &Fixture{}
}

func (f *Fixture) Azure() *AzureFixture {
	return &AzureFixture{}
}

func (f *Fixture) Kubernetes() *KubernetesFixture {
	return &KubernetesFixture{}
}

func (f *Fixture) RandomUint32(n uint32) uint32 {
	return uint32(f.RandomUint64(uint64(n)))
}

func (f *Fixture) RandomUint64(n uint64) uint64 {
	m := big.NewInt(0)
	m.SetUint64(n)
	rv, err := rand.Int(rand.Reader, m)
	if err != nil {
		panic(fmt.Sprintf("unreachable: %v", err))
	}
	return rv.Uint64()
}

func (f *Fixture) RandomIPv4Addresses(n int) []netip.Addr {

	rv := make([]netip.Addr, 0, n)
	for i := 0; i < n; i++ {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], f.RandomUint32(math.MaxUint32))
		buf[0] = uint8(i + 1) // to keep them unique
		rv = append(rv, netip.AddrFrom4(buf))
	}

	return rv
}

func (f *Fixture) RandomIPv4AddressStrings(n int) []string {
	return fnutil.Map(func(p netip.Addr) string { return p.String() }, f.RandomIPv4Addresses(n))
}

func (f *Fixture) RandomIPv4Prefixes(n int) []netip.Prefix {
	var (
		rv  = make([]netip.Prefix, 0, n)
		ips = f.RandomIPv4Addresses(n)
	)

	for _, ip := range ips {
		bits := int(f.RandomUint64(24)) + 8
		p, err := ip.Prefix(bits)
		if err != nil {
			panic("unreachable: it should always be a valid IP prefix")
		}
		rv = append(rv, p)
	}

	return rv
}

func (f *Fixture) RandomIPv4PrefixStrings(n int) []string {
	return fnutil.Map(func(p netip.Prefix) string { return p.String() }, f.RandomIPv4Prefixes(n))
}

func (f *Fixture) RandomIPv6Addresses(n int) []netip.Addr {
	rv := make([]netip.Addr, 0, n)
	for i := 0; i < n; i++ {
		var buf [16]byte
		binary.BigEndian.PutUint64(buf[:8], f.RandomUint64(math.MaxUint64))
		binary.BigEndian.PutUint64(buf[8:], f.RandomUint64(math.MaxUint64))
		buf[0] = uint8(i + 1) // to keep them unique
		rv = append(rv, netip.AddrFrom16(buf))
	}

	return rv
}

func (f *Fixture) RandomIPv6AddressStrings(n int) []string {
	return fnutil.Map(func(p netip.Addr) string { return p.String() }, f.RandomIPv6Addresses(n))
}

func (f *Fixture) RandomIPv6Prefixes(n int) []netip.Prefix {
	var (
		rv  = make([]netip.Prefix, 0, n)
		ips = f.RandomIPv6Addresses(n)
	)

	for _, ip := range ips {
		bits := int(f.RandomUint64(120)) + 8
		p, err := ip.Prefix(bits)
		if err != nil {
			panic("unreachable: it should always be a valid IP prefix")
		}
		rv = append(rv, p)
	}

	return rv
}

func (f *Fixture) RandomIPv6PrefixStrings(n int) []string {
	return fnutil.Map(func(p netip.Prefix) string { return p.String() }, f.RandomIPv6Prefixes(n))
}
