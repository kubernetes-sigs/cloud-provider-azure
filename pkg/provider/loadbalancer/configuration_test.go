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

package loadbalancer

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestIsInternal(t *testing.T) {
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
				},
			},
		}
		assert.True(t, IsInternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "TRUE",
				},
			},
		}
		assert.True(t, IsInternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "foobar",
				},
			},
		}
		assert.False(t, IsInternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		}
		assert.False(t, IsInternal(&svc))
	}
}

func TestAllowedServiceTags(t *testing.T) {
	t.Run("no annotation", func(t *testing.T) {
		actual, err := AllowedServiceTags(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		})
		assert.NoError(t, err)
		assert.Empty(t, actual)
	})
	t.Run("with 1 service tag", func(t *testing.T) {
		actual, err := AllowedServiceTags(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedServiceTags: "Microsoft.ContainerInstance/containerGroups",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []string{"Microsoft.ContainerInstance/containerGroups"}, actual)
	})
	t.Run("with multiple service tags", func(t *testing.T) {
		actual, err := AllowedServiceTags(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedServiceTags: " Microsoft.ContainerInstance/containerGroups, foo, bar ",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []string{"Microsoft.ContainerInstance/containerGroups", "foo", "bar"}, actual)
	})
}

func TestAllowedIPRanges(t *testing.T) {
	t.Run("no annotation", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		})
		assert.NoError(t, err)
		assert.Empty(t, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with 1 IPv4 range in allowed ip ranges", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges: "10.10.0.0/24",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.10.0.0/24")}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with 1 IPv4 range in load balancer source ranges", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.AnnotationLoadBalancerSourceRangesKey: "10.10.0.0/24",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.10.0.0/24")}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with 1 IPv6 range in allowed ip ranges", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges: "2001:db8::/32",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with 1 IPv6 range in load balancer source ranges", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.AnnotationLoadBalancerSourceRangesKey: "2001:db8::/32",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with multiple IP ranges", func(t *testing.T) {
		actual, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges:  " 10.10.0.0/24, 2001:db8::/32 ",
					v1.AnnotationLoadBalancerSourceRangesKey: " 10.20.0.0/24, 2002:db8::/32 ",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.0.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
			netip.MustParsePrefix("10.20.0.0/24"),
			netip.MustParsePrefix("2002:db8::/32"),
		}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with invalid IP range", func(t *testing.T) {
		_, invalid, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges:  "foobar,10.0.0.1/24",
					v1.AnnotationLoadBalancerSourceRangesKey: "barfoo,2002:db8::1/32",
				},
			},
		})
		assert.Error(t, err)

		var e *ErrAnnotationValue
		assert.ErrorAs(t, err, &e)
		assert.Equal(t, []string{"foobar", "10.0.0.1/24", "barfoo", "2002:db8::1/32"}, invalid)
	})
}

func TestSourceRanges(t *testing.T) {
	t.Run("not specified in spec", func(t *testing.T) {
		actual, invalid, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
		})
		assert.NoError(t, err)
		assert.Empty(t, actual)
		assert.Empty(t, invalid)
	})
	t.Run("specified in spec", func(t *testing.T) {
		actual, invalid, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type:                     v1.ServiceTypeLoadBalancer,
				LoadBalancerSourceRanges: []string{"10.10.0.0/24", "2001:db8::/32"},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.0.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		}, actual)
		assert.Empty(t, invalid)
	})
	t.Run("with invalid IP range in spec", func(t *testing.T) {
		_, invalid, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type:                     v1.ServiceTypeLoadBalancer,
				LoadBalancerSourceRanges: []string{"foobar", "10.0.0.1/24"},
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		})
		assert.Error(t, err)
		assert.Equal(t, []string{"foobar", "10.0.0.1/24"}, invalid)
	})
}

func TestAdditionalPublicIPs(t *testing.T) {
	t.Run("no annotation", func(t *testing.T) {
		actual, err := AdditionalPublicIPs(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		})
		assert.NoError(t, err)
		assert.Empty(t, actual)
	})
	t.Run("with 1 IPv4", func(t *testing.T) {
		actual, err := AdditionalPublicIPs(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAdditionalPublicIPs: "10.10.0.1",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.10.0.1")}, actual)
	})
	t.Run("with 1 IPv6", func(t *testing.T) {
		actual, err := AdditionalPublicIPs(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAdditionalPublicIPs: "2001:db8::1",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("2001:db8::1")}, actual)
	})
	t.Run("with multiple IPs", func(t *testing.T) {
		actual, err := AdditionalPublicIPs(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAdditionalPublicIPs: "10.10.0.1,2001:db8::1",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Addr{
			netip.MustParseAddr("10.10.0.1"),
			netip.MustParseAddr("2001:db8::1"),
		}, actual)
	})
	t.Run("with invalid IP", func(t *testing.T) {
		_, err := AdditionalPublicIPs(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAdditionalPublicIPs: "foobar",
				},
			},
		})
		assert.Error(t, err)

		var e *ErrAnnotationValue
		assert.ErrorAs(t, err, &e)
		assert.Equal(t, e.AnnotationKey, consts.ServiceAnnotationAdditionalPublicIPs)
	})
}
