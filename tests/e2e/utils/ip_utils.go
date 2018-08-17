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
	"strconv"
	"strings"
)

const (
	leastPrefix = 24
)

// ValidateIPInCIDR validates whether certain ip fits CIDR
func ValidateIPInCIDR(ip, cidr string) (bool, error) {
	ipTemp := ip + "/24"
	ipInt, _, err := cidrString2intArray(ipTemp)
	if err != nil {
		return false, err
	}
	cidrInt, mask, err := cidrString2intArray(cidr)
	if err != nil {
		return false, err
	}
	for i := 0; i < mask; i++ {
		if cidrInt[i] != ipInt[i] {
			return false, nil
		}
	}
	return true, nil
}

func getNextSubnet(vnetCIDR string, existSubnets []string) (string, error) {
	intIPArray, vNetMask, err := cidrString2intArray(vnetCIDR)
	if err != nil {
		return "", fmt.Errorf("Unexpected vnet address CIDR")
	}
	root := initIPTreeRoot(vNetMask)
	for _, subnet := range existSubnets {
		subnetIPArray, subnetPrefix, err := cidrString2intArray(subnet)
		if err != nil {
			return "", fmt.Errorf("Unexpected subnet address prefix")
		}
		setOccupiedByPrefix(root, subnetIPArray, subnetPrefix)
	}
	retArray, retMask := findNodeUsable(root, intIPArray)
	ret := prefixIntArray2String(retArray, retMask)
	return ret, nil
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
		err = fmt.Errorf("It is not a valid CIDR format")
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
