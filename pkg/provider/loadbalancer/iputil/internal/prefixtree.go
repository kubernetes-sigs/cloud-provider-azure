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
	"net/netip"
)

type prefixTreeNode struct {
	masked bool
	prefix netip.Prefix

	p *prefixTreeNode // parent node
	l *prefixTreeNode // left child node
	r *prefixTreeNode // right child node
}

// NewLeftChild creates a new left child node for the current node.
// No checks are performed to see if the child already exists.
func (n *prefixTreeNode) NewLeftChild() *prefixTreeNode {
	prefix := netip.PrefixFrom(n.prefix.Addr(), n.prefix.Bits()+1)
	n.l = &prefixTreeNode{
		prefix: prefix,
		p:      n,
	}
	return n.l
}

// NewRightChild creates a new right child node for the current node.
// No checks are performed to see if the child already exists.
func (n *prefixTreeNode) NewRightChild() *prefixTreeNode {
	prefixBytes := n.prefix.Addr().AsSlice()
	{
		// Set the next bit to 1 for the new prefix (it's the right child)
		byteIndex := n.prefix.Bits() / 8
		bitIndex := n.prefix.Bits() % 8
		prefixBytes[byteIndex] |= 1 << (7 - bitIndex)
	}

	addr, _ := netip.AddrFromSlice(prefixBytes)
	prefix := netip.PrefixFrom(addr, n.prefix.Bits()+1)
	n.r = &prefixTreeNode{
		prefix: prefix,
		p:      n,
	}
	return n.r
}

// CondenseUntilRoot checks if the current node and its sibling are masked,
// and if so, marks their parent as masked and removes both children.
// This process is repeated up the tree until a node with an unmasked sibling is found.
//
// The process can be visualized as follows:
//
//	Before:           After:
//	   P                 P (masked)
//	  / \               / \
//	 A   B     ->      X   X
//	(M) (M)
//
// Where:
//
//	P: Parent node
//	A, B: Child nodes
//	M: Masked
//	X: Removed
//
// This method helps to optimize the tree structure by condensing fully masked subtrees.
func (n *prefixTreeNode) CondenseUntilRoot() {
	var node = n
	for node.p != nil {
		p := node.p
		if p.l == nil || !p.l.masked {
			break
		}
		if p.r == nil || !p.r.masked {
			break
		}
		p.masked = true
		p.l, p.r = nil, nil
		node = p
	}
}

// PrefixTree represents a tree structure for storing and managing IP prefixes.
// It efficiently handles prefix aggregation, merging of overlapping prefixes,
// and collapsing of neighboring prefixes.
//
// The tree is structured as follows:
// - Each node represents a bit in the IP address
// - Left child represents a 0 bit, right child represents a 1 bit
// - Masked nodes indicate the end of a prefix
// - Unused branches are represented by nil pointers
//
// Example tree for 128.0.0.0/4 (binary 1000 0000):
//
//	    0 (0.0.0.0/0)
//	   / \
//	  X   1 (128.0.0.0/1)
//	     / \
//	    0   X
//	   / \
//	  0   X
//	 / \
//	0*  X
//
// Where:
// * denotes a masked node (prefix end)
// X denotes an unused branch (nil pointer)
type PrefixTree struct {
	maxBits int
	root    *prefixTreeNode
}

// NewPrefixTreeForIPv4 creates a new prefix tree for IPv4 addresses.
// The max depth of the tree is 32 + 1 (for the root).
func NewPrefixTreeForIPv4() *PrefixTree {
	return &PrefixTree{
		maxBits: 32,
		root: &prefixTreeNode{
			prefix: netip.MustParsePrefix("0.0.0.0/0"),
		},
	}
}

// NewPrefixTreeForIPv6 creates a new prefix tree for IPv6 addresses.
// The max depth of the tree is 128 + 1 (for the root).
func NewPrefixTreeForIPv6() *PrefixTree {
	return &PrefixTree{
		maxBits: 128,
		root: &prefixTreeNode{
			prefix: netip.MustParsePrefix("::/0"),
		},
	}
}

// Add adds a prefix to the tree.
// It will merge overlapping prefixes and collapse neighboring prefixes if possible.
func (t *PrefixTree) Add(prefix netip.Prefix) {
	var (
		n     = t.root
		bytes = prefix.Addr().AsSlice()
	)
	for i := 0; i < prefix.Bits(); i++ {
		if n.masked {
			break // It's already masked, the rest of the bits are irrelevant
		}

		if bitAt(bytes, i) == 0 {
			if n.l == nil {
				n.NewLeftChild()
			}
			n = n.l
		} else {
			if n.r == nil {
				n.NewRightChild()
			}
			n = n.r
		}
	}

	n.masked = true
	n.l, n.r = nil, nil
	n.CondenseUntilRoot()
}

// Remove removes a prefix from the tree.
// If the prefix is not in the tree, it does nothing.
func (t *PrefixTree) Remove(prefix netip.Prefix) {
	var (
		n     = t.root
		bytes = prefix.Addr().AsSlice()
	)

	isMasked := false
	for i := 0; n != nil && i < prefix.Bits(); i++ {
		var bit = bitAt(bytes, i)

		if !n.masked && !isMasked {
			// Keep going down until it finds a masked node
			if bit == 0 {
				n = n.l
			} else {
				n = n.r
			}
			continue
		}

		isMasked = true
		n.masked = false

		// If the node is masked, it should have no children,
		// and we need to split it. The other side should be masked.
		n.NewLeftChild()
		n.NewRightChild()
		if bit == 0 {
			n.r.masked = true
			n = n.l
		} else {
			n.l.masked = true
			n = n.r
		}
	}
	if n != nil {
		n.masked = false
	}
}

// List returns all prefixes in the tree.
// Overlapping prefixes are merged.
// It will also collapse the neighboring prefixes.
// The order of the prefixes in the output is guaranteed.
//
// Example:
//   - [192.168.0.0/16, 192.168.1.0/24, 192.168.0.1/32] -> [192.168.0.0/16]
//   - [192.168.0.0/32, 192.168.0.1/32] -> [192.168.0.0/31]
func (t *PrefixTree) List() []netip.Prefix {
	var (
		rv []netip.Prefix
		q  = []*prefixTreeNode{t.root}
	)

	for len(q) > 0 {
		n := q[len(q)-1]
		q = q[:len(q)-1]

		if n.masked {
			rv = append(rv, n.prefix)
			continue
		}

		if n.l != nil {
			q = append(q, n.l)
		}
		if n.r != nil {
			q = append(q, n.r)
		}
	}

	return rv
}
