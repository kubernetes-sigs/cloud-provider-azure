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

func TestIsExternal(t *testing.T) {
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
				},
			},
		}
		assert.False(t, IsExternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "TRUE",
				},
			},
		}
		assert.False(t, IsExternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "foobar",
				},
			},
		}
		assert.True(t, IsExternal(&svc))
	}
	{
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		}
		assert.True(t, IsExternal(&svc))
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
					consts.ServiceAnnotationAllowedServiceTags: "Microsoft.ContainerInstance/containerGroups,foo,bar",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []string{"Microsoft.ContainerInstance/containerGroups", "foo", "bar"}, actual)
	})
}

func TestAllowedIPRanges(t *testing.T) {
	t.Run("no annotation", func(t *testing.T) {
		actual, err := AllowedIPRanges(&v1.Service{
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
	t.Run("with 1 IPv4 range", func(t *testing.T) {
		actual, err := AllowedIPRanges(&v1.Service{
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
	})
	t.Run("with 1 IPv6 range", func(t *testing.T) {
		actual, err := AllowedIPRanges(&v1.Service{
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
	})
	t.Run("with multiple IP ranges", func(t *testing.T) {
		actual, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges: "10.10.0.0/24,2001:db8::/32",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.0.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		}, actual)
	})
	t.Run("with invalid IP range", func(t *testing.T) {
		_, err := AllowedIPRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges: "foobar",
				},
			},
		})
		assert.Error(t, err)
	})
}

func TestSourceRanges(t *testing.T) {
	t.Run("not specified in spec", func(t *testing.T) {
		actual, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
		})
		assert.NoError(t, err)
		assert.Empty(t, actual)
	})
	t.Run("specified in spec", func(t *testing.T) {
		actual, err := SourceRanges(&v1.Service{
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
	})
	t.Run("specified in annotation", func(t *testing.T) {
		actual, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.AnnotationLoadBalancerSourceRangesKey: "10.10.0.0/24,2001:db8::/32",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.0.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		}, actual)
	})
	t.Run("specified in both spec and annotation", func(t *testing.T) {
		actual, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type:                     v1.ServiceTypeLoadBalancer,
				LoadBalancerSourceRanges: []string{"10.10.0.0/24"},
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.AnnotationLoadBalancerSourceRangesKey: "2001:db8::/32",
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.0.0/24"),
		}, actual, "spec should take precedence over annotation")
	})
	t.Run("with invalid IP range", func(t *testing.T) {
		_, err := SourceRanges(&v1.Service{
			Spec: v1.ServiceSpec{
				Type:                     v1.ServiceTypeLoadBalancer,
				LoadBalancerSourceRanges: []string{"foobar"},
			},
		})
		assert.Error(t, err)
	})
}

func TestAccessControl_IsAllowFromInternet(t *testing.T) {
	t.Run("external LB", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		}
		t.Run("default", func(t *testing.T) {
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.True(t, ac.IsAllowFromInternet())
		})
		t.Run("not allowed from all", func(t *testing.T) {
			svc.Spec.LoadBalancerSourceRanges = []string{"10.10.10.0/24"}
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.False(t, ac.IsAllowFromInternet())
		})
		t.Run("allowed from all", func(t *testing.T) {
			svc.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0"}
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.True(t, ac.IsAllowFromInternet())
		})
	})

	t.Run("internal LB", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
				},
			},
		}
		t.Run("default", func(t *testing.T) {
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.False(t, ac.IsAllowFromInternet())
		})
		t.Run("not allowed from all", func(t *testing.T) {
			svc.Spec.LoadBalancerSourceRanges = []string{"10.10.10.0/24"}
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.False(t, ac.IsAllowFromInternet())
		})
		t.Run("allowed from all", func(t *testing.T) {
			svc.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0"}
			ac, err := NewAccessControl(&svc)
			assert.NoError(t, err)
			assert.True(t, ac.IsAllowFromInternet())
		})
	})
}

func TestAccessControl_IPV4Sources(t *testing.T) {
	t.Run("external LB", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges:    "10.10.10.0/24,192.168.0.1/32,2001:db8::/32,2002:db8::/32",
					consts.ServiceAnnotationAllowedServiceTags: "foo,bar",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"10.10.10.0/24",
			"192.168.0.1/32",
			"foo",
			"bar",
		}, ac.IPV4Sources())
	})
	t.Run("internal LB with Internet access", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
					consts.ServiceAnnotationAllowedIPRanges:      "0.0.0.0/0",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"Internet",
			"0.0.0.0/0",
		}, ac.IPV4Sources())
	})
	t.Run("internal LB without Internet access", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
					consts.ServiceAnnotationAllowedIPRanges:      "10.10.10.0/24,192.168.0.1/32,2001:db8::/32,2002:db8::/32",
					consts.ServiceAnnotationAllowedServiceTags:   "foo,bar",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"10.10.10.0/24",
			"192.168.0.1/32",
			"foo",
			"bar",
		}, ac.IPV4Sources())
	})
}

func TestAccessControl_IPV6Sources(t *testing.T) {
	t.Run("external LB", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges:    "10.10.10.0/24,192.168.0.1/32,2001:db8::/32,2002:db8::/32",
					consts.ServiceAnnotationAllowedServiceTags: "foo,bar",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"2001:db8::/32",
			"2002:db8::/32",
			"foo",
			"bar",
		}, ac.IPV6Sources())
	})
	t.Run("internal LB with Internet access", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
					consts.ServiceAnnotationAllowedIPRanges:      "::/0",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"Internet",
			"::/0",
		}, ac.IPV6Sources())
	})
	t.Run("internal LB without Internet access", func(t *testing.T) {
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: "true",
					consts.ServiceAnnotationAllowedIPRanges:      "10.10.10.0/24,192.168.0.1/32,2001:db8::/32,2002:db8::/32",
					consts.ServiceAnnotationAllowedServiceTags:   "foo,bar",
				},
			},
		}
		ac, err := NewAccessControl(&svc)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"2001:db8::/32",
			"2002:db8::/32",
			"foo",
			"bar",
		}, ac.IPV6Sources())
	})
}
