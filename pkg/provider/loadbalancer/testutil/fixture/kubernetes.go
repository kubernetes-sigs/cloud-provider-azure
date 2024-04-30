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
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

type KubernetesFixture struct{}

func (f *KubernetesFixture) Service() *KubernetesServiceFixture {
	return &KubernetesServiceFixture{
		svc: v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "foo",
				Annotations: make(map[string]string),
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{
					{
						Name:     "http",
						Protocol: v1.ProtocolTCP,
						Port:     80,
						NodePort: 50080,
					},
					{
						Name:     "https",
						Protocol: v1.ProtocolTCP,
						Port:     443,
						NodePort: 50443,
					},
					{
						Name:     "dns-tcp",
						Protocol: v1.ProtocolTCP,
						Port:     53,
						NodePort: 50053,
					},
					{
						Name:     "dns-udp",
						Protocol: v1.ProtocolUDP,
						Port:     53,
						NodePort: 50053,
					},
				},
			},
		},
	}
}

type KubernetesServiceFixture struct {
	svc v1.Service
}

func (f *KubernetesServiceFixture) WithNamespace(ns string) *KubernetesServiceFixture {
	f.svc.Namespace = ns
	return f
}

func (f *KubernetesServiceFixture) WithName(name string) *KubernetesServiceFixture {
	f.svc.Name = name
	return f
}

func (f *KubernetesServiceFixture) WithInternalEnabled() *KubernetesServiceFixture {
	f.svc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = "true"
	return f
}

func (f *KubernetesServiceFixture) WithDenyAllExceptLoadBalancerSourceRanges() *KubernetesServiceFixture {
	f.svc.Annotations[consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges] = "true"
	return f
}

func (f *KubernetesServiceFixture) WithAllowedIPRanges(parts ...string) *KubernetesServiceFixture {
	f.svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = strings.Join(parts, ",")
	return f
}

func (f *KubernetesServiceFixture) WithAllowedServiceTags(parts ...string) *KubernetesServiceFixture {
	f.svc.Annotations[consts.ServiceAnnotationAllowedServiceTags] = strings.Join(parts, ",")
	return f
}

func (f *KubernetesServiceFixture) WithLoadBalancerSourceRanges(parts ...string) *KubernetesServiceFixture {
	f.svc.Spec.LoadBalancerSourceRanges = parts
	return f
}

func (f *KubernetesServiceFixture) TCPPorts() []int32 {
	var rv []int32
	for _, p := range f.svc.Spec.Ports {
		if p.Protocol == v1.ProtocolTCP {
			rv = append(rv, p.Port)
		}
	}
	return rv
}

func (f *KubernetesServiceFixture) UDPPorts() []int32 {
	var rv []int32
	for _, p := range f.svc.Spec.Ports {
		if p.Protocol == v1.ProtocolUDP {
			rv = append(rv, p.Port)
		}
	}
	return rv
}

func (f *KubernetesServiceFixture) TCPNodePorts() []int32 {
	var rv []int32
	for _, p := range f.svc.Spec.Ports {
		if p.Protocol == v1.ProtocolTCP {
			rv = append(rv, p.NodePort)
		}
	}
	return rv
}

func (f *KubernetesServiceFixture) UDPNodePorts() []int32 {
	var rv []int32
	for _, p := range f.svc.Spec.Ports {
		if p.Protocol == v1.ProtocolUDP {
			rv = append(rv, p.NodePort)
		}
	}
	return rv
}

func (f *KubernetesServiceFixture) Build() v1.Service {
	return f.svc
}
