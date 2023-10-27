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

package utils

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	leastPrefix = 24
)

func getNextSubnet(vnetCIDR *string, existSubnets []*string) (*net.IPNet, error) {
	// Filter existSubnets with IP family
	isIPv6, err := isCIDRIPv6(vnetCIDR)
	if err != nil {
		return nil, err
	}
	existSubnetsWithSameIPFamily := []string{}
	for _, subnet := range existSubnets {
		isSubnetIPv6, err := isCIDRIPv6(subnet)
		if err != nil {
			return nil, err
		}
		if isSubnetIPv6 == isIPv6 {
			existSubnetsWithSameIPFamily = append(existSubnetsWithSameIPFamily, *subnet)
		}
	}

	// IPv6
	// TODO: A better solution to replace this hack.
	if isIPv6 {
		ip, ipNet, _ := net.ParseCIDR(*vnetCIDR)
		mask := ipNet.Mask
		pos := 0
		for i := range ip {
			if mask[i] != 255 {
				pos = i
				break
			}
		}
		found := false
		for i := 255; i > 0; i-- {
			ip[pos] = byte(i)
			// Check if the subnet is in any existing one
			contains := false
			for _, existSubnet := range existSubnetsWithSameIPFamily {
				_, existSubnetIPNet, err := net.ParseCIDR(existSubnet)
				if err != nil {
					return nil, err
				}
				if existSubnetIPNet.Contains(ip) {
					contains = true
					break
				}
			}
			if !contains {
				found = true
				break
			}

		}
		if !found {
			return nil, fmt.Errorf("failed to find the next subnet: vnetCIDR: %s, existsSubnets: %v",
				*vnetCIDR, existSubnets)
		}
		// Prefix length can only be 64 in case of IPv6 address prefixes in subnets.
		_, nextSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/64", ip))
		return nextSubnet, err
	}

	// IPv4
	intIPArray, vNetMask, err := cidrString2intArray(*vnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("unexpected vnet address CIDR: %w", err)
	}
	root := initIPTreeRoot(vNetMask)
	for _, subnet := range existSubnetsWithSameIPFamily {
		subnetIPArray, subnetPrefix, err := cidrString2intArray(subnet)
		if err != nil {
			return nil, fmt.Errorf("unexpected subnet address prefix: %w", err)
		}
		setOccupiedByPrefix(root, subnetIPArray, subnetPrefix)
	}
	retArray, retMask := findNodeUsable(root, intIPArray)
	ret := prefixIntArray2String(retArray, retMask)
	_, retSubnet, err := net.ParseCIDR(ret)
	return retSubnet, err
}

type ipNode struct {
	Occupied bool
	Usable   bool
	Depth    int
	Left     *ipNode // bit 0
	Right    *ipNode // bit 1
}

func newIPNode(depth int) *ipNode {
	root := &ipNode{
		Occupied: false,
		Usable:   true,
		Depth:    depth,
		Left:     nil,
		Right:    nil,
	}
	if depth < leastPrefix {
		root.Usable = false
	}
	return root
}

func initIPTreeRoot(depth int) *ipNode {
	if depth >= 32 {
		return nil
	}
	root := newIPNode(depth)
	root.Left = initIPTreeRoot(depth + 1)
	root.Right = initIPTreeRoot(depth + 1)
	return root
}

func setOccupiedByPrefix(root *ipNode, ip []int, prefix int) {
	if root == nil {
		return
	}
	root.Usable = false // Node passed cannot be used as subnet
	if root.Depth == prefix {
		root.Occupied = true
		return
	}
	if root.Depth > prefix {
		return
	}
	if ip[root.Depth] == 1 {
		setOccupiedByPrefix(root.Right, ip, prefix)
	} else {
		setOccupiedByPrefix(root.Left, ip, prefix)
	}
}

func findNodeUsable(root *ipNode, ip []int) ([]int, int) {
	if root == nil {
		return ip, -1
	}
	if root.Depth >= 32 || root.Occupied {
		return ip, -1 // at least remain 1 suffix for subnet
	}
	if root.Usable {
		return ip, root.Depth
	}
	var mask int
	ip[root.Depth] = 0
	ip, mask = findNodeUsable(root.Left, ip)
	if mask > 0 {
		return ip, mask
	}
	ip[root.Depth] = 1
	ip, mask = findNodeUsable(root.Right, ip)
	return ip, mask
}

func prefixIntArray2String(ret []int, prefix int) (ip string) {
	ip = ""
	if prefix < 0 {
		return ip
	}
	for i := 0; i < 4; i++ {
		temp := 0
		for j := 0; j < 8; j++ {
			temp += ret[i*8+j] << uint(7-j)
		}
		ip += strconv.Itoa(temp)
		if i != 3 {
			ip += "."
		}
	}
	ip += "/" + strconv.Itoa(prefix)
	return
}

func cidrString2intArray(ip string) (ret []int, prefix int, err error) {
	splitPos := strings.Index(ip, "/")
	if splitPos == -1 {
		err = fmt.Errorf("it is not a valid CIDR format")
	} else {
		prefix, err = strconv.Atoi(ip[splitPos+1:])
	}
	if err != nil {
		return
	}
	for _, ipSection := range strings.Split(ip[0:splitPos], ".") {
		var section int
		section, err = strconv.Atoi(ipSection)
		if err != nil {
			return
		}
		for i := 7; i >= 0; i-- {
			ret = append(ret, section>>uint(i)&1)
		}
	}
	return
}
