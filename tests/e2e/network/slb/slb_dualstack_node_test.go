/*
Copyright 2026 The Kubernetes Authors.

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

package network

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	utilnet "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// Regression coverage for single-stack (IPv4 OR IPv6) LoadBalancer services whose backend pods run
// on DUAL-STACK nodes. On a dual-stack node the cloud provider must register each pod IP under the
// node InternalIP of the MATCHING family (the underlay address ServiceGateway routes through). A
// family-blind location key would land an IPv6 pod under the node's IPv4 underlay IP, breaking
// routing and orphaning the IPv6 location across a CCM restart.
//
// The whole suite skips cleanly on a cluster whose nodes are not dual-stack (no node exposes both
// an IPv4 and an IPv6 InternalIP), so it is safe to run everywhere.
// ipEqual reports whether two textual IP addresses denote the same address.
// The Service Gateway API may return an IPv6 address in a different letter case
// or representation than the Kubernetes node InternalIP, so address and location
// keys are compared by parsed value rather than by raw string.
func ipEqual(a, b string) bool {
	parsed := net.ParseIP(a)
	return parsed != nil && parsed.Equal(net.ParseIP(b))
}

var _ = Describe("SLB - Dual-stack nodes with single-stack services", Label(slbTestLabel), func() {
	basename := "slb-dualstack-node-test"

	var (
		cs clientset.Interface
		ns *v1.Namespace
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			Expect(utils.DeleteNamespace(cs, ns.Name)).To(Succeed())
			By("Waiting for Azure cleanup")
			eventuallyAzureCleanup(2 * time.Minute)
			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()
			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	// dualStackNodeIPs returns nodeName -> {ipv4, ipv6} for nodes that expose BOTH an IPv4 and an
	// IPv6 InternalIP. Returns an empty map on a single-family cluster.
	dualStackNodeIPs := func() map[string][2]string {
		nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		out := make(map[string][2]string)
		for _, node := range nodes.Items {
			var v4, v6 string
			for _, addr := range node.Status.Addresses {
				if addr.Type != v1.NodeInternalIP {
					continue
				}
				if utilnet.IsIPv6String(addr.Address) {
					v6 = addr.Address
				} else {
					v4 = addr.Address
				}
			}
			if v4 != "" && v6 != "" {
				out[node.Name] = [2]string{v4, v6}
			}
		}
		return out
	}

	makeNetexecPodOnNode := func(name, nodeName string, labels map[string]string, targetPort int) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
			Spec: v1.PodSpec{
				NodeName: nodeName, // pin so we know which node's underlay IP to expect
				Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}},
			},
		}
	}

	newSingleStackLBService := func(name string, family v1.IPFamily, labels map[string]string, port int32, targetPort int) *v1.Service {
		singleStack := v1.IPFamilyPolicySingleStack
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:           v1.ServiceTypeLoadBalancer,
				IPFamilyPolicy: &singleStack,
				IPFamilies:     []v1.IPFamily{family},
				Selector:       labels,
				Ports: []v1.ServicePort{{
					Port:       port,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
	}

	// sgLocationForAddress returns the addressLocation (node underlay IP) under which podIP is
	// registered in the Service Gateway, or "" if not found.
	sgLocationForAddress := func(podIP string) string {
		resp, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())
		for _, loc := range resp.Value {
			for _, addr := range loc.Addresses {
				if ipEqual(addr.Address, podIP) {
					return loc.AddressLocation
				}
			}
		}
		return ""
	}

	It("registers IPv4 and IPv6 single-stack service pods under their family-matched node location", func() {
		const (
			servicePort  = int32(80)
			targetPort   = 8080
			provisioning = 3 * time.Minute
		)

		dsNodes := dualStackNodeIPs()
		if len(dsNodes) == 0 {
			Skip("no dual-stack nodes (need a node exposing both an IPv4 and an IPv6 InternalIP)")
		}
		var nodeName string
		var nodeV4, nodeV6 string
		for n, ips := range dsNodes {
			nodeName, nodeV4, nodeV6 = n, ips[0], ips[1]
			break
		}
		utils.Logf("Using dual-stack node %q (IPv4=%s, IPv6=%s)", nodeName, nodeV4, nodeV6)

		// --- IPv4 single-stack service ---
		v4Labels := map[string]string{"app": "ds-v4-app"}
		v4Svc := newSingleStackLBService("ds-v4-svc", v1.IPv4Protocol, v4Labels, servicePort, targetPort)
		createdV4, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), v4Svc, metav1.CreateOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		if len(createdV4.Spec.IPFamilies) == 0 || createdV4.Spec.IPFamilies[0] != v1.IPv4Protocol {
			Skip("cluster did not honor an IPv4 single-stack request")
		}
		v4UID := string(createdV4.UID)

		// --- IPv6 single-stack service ---
		v6Labels := map[string]string{"app": "ds-v6-app"}
		v6Svc := newSingleStackLBService("ds-v6-svc", v1.IPv6Protocol, v6Labels, servicePort, targetPort)
		createdV6, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), v6Svc, metav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "IPv6") || strings.Contains(err.Error(), "ipFamilies") || strings.Contains(err.Error(), "family") {
				Skip("cluster does not support IPv6 services: " + err.Error())
			}
			Expect(err).NotTo(HaveOccurred())
		}
		if len(createdV6.Spec.IPFamilies) == 0 || createdV6.Spec.IPFamilies[0] != v1.IPv6Protocol {
			Skip("cluster did not honor an IPv6 single-stack request")
		}
		v6UID := string(createdV6.UID)

		By("Creating one backend pod per service, both pinned to the same dual-stack node")
		_, err = cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPodOnNode("ds-v4-pod", nodeName, v4Labels, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPodOnNode("ds-v6-pod", nodeName, v6Labels, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for both services to register their pod in Service Gateway")
		eventuallyServiceReconciled(v4UID, 1, provisioning)
		eventuallyServiceReconciled(v6UID, 1, provisioning)

		By("Reading back the pods' actual IPs")
		v4Pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), "ds-v4-pod", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		v6Pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), "ds-v6-pod", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		v4PodIP := podIPOfFamily(v4Pod, false)
		v6PodIP := podIPOfFamily(v6Pod, true)
		Expect(v4PodIP).NotTo(BeEmpty(), "v4 backend pod must have an IPv4 PodIP")
		Expect(v6PodIP).NotTo(BeEmpty(), "v6 backend pod must have an IPv6 PodIP")
		utils.Logf("v4 pod IP=%s (expect location %s), v6 pod IP=%s (expect location %s)", v4PodIP, nodeV4, v6PodIP, nodeV6)

		By("Asserting each pod IP is registered under its family-matched node underlay location")
		Eventually(func() error {
			gotV4 := sgLocationForAddress(v4PodIP)
			if !ipEqual(gotV4, nodeV4) {
				return fmt.Errorf("IPv4 pod %s registered under location %q, want node IPv4 underlay %q", v4PodIP, gotV4, nodeV4)
			}
			gotV6 := sgLocationForAddress(v6PodIP)
			if !ipEqual(gotV6, nodeV6) {
				return fmt.Errorf("IPv6 pod %s registered under location %q, want node IPv6 underlay %q", v6PodIP, gotV6, nodeV6)
			}
			return nil
		}, provisioning, 10*time.Second).Should(Succeed(),
			"each single-stack service's pod must register under the node InternalIP of its own family")

		// The two families must occupy DISTINCT location keys even though both pods are on one node.
		Expect(nodeV4).NotTo(Equal(nodeV6))
		utils.Logf("\n✓ Dual-stack node: v4 pod under %s, v6 pod under %s (family-partitioned)", nodeV4, nodeV6)

		By("Asserting the IPv6 location key is stable across reconciles (no representation churn)")
		// A non-canonical IPv6 key on the K8s side would diff against the canonical NRP location and
		// surface as the address/location flapping between reconciles; the key must stay constant.
		stableV6Loc := sgLocationForAddress(v6PodIP)
		Expect(stableV6Loc).NotTo(BeEmpty())
		Consistently(func() string {
			return sgLocationForAddress(v6PodIP)
		}, 20*time.Second, 5*time.Second).Should(Equal(stableV6Loc),
			"the IPv6 pod's registered node location must remain a single stable key across reconciles")
	})
})

// podIPOfFamily returns the pod IP of the requested family (ipv6=true for IPv6) from PodIPs,
// falling back to PodIP. Returns "" if none of the requested family exists.
func podIPOfFamily(pod *v1.Pod, ipv6 bool) string {
	for _, pip := range pod.Status.PodIPs {
		if utilnet.IsIPv6String(pip.IP) == ipv6 {
			return pip.IP
		}
	}
	if pod.Status.PodIP != "" && utilnet.IsIPv6String(pod.Status.PodIP) == ipv6 {
		return pod.Status.PodIP
	}
	return ""
}
