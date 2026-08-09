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

package servicegateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// natGatewayPublicIPRefs returns the Public IP resource names attached to an egress NAT Gateway,
// split by the ARM property each is attached to.
//
// The NAT Gateway is a ServiceGateway/NRP-managed resource, so it is read with `az rest` at the
// ServiceGateway API version: `az network nat gateway show` deserializes its properties as empty
// and would make every assertion below trivially unobservable.
func natGatewayPublicIPRefs(egressName string) (v4Names, v6Names []string, err error) {
	natID := ""
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return nil, nil, err
	}
	for _, svc := range sgResponse.Value {
		if svc.Properties.ServiceType == "Outbound" && strings.EqualFold(svc.Name, egressName) {
			natID = svc.Properties.PublicNatGatewayID
			break
		}
	}
	if natID == "" {
		return nil, nil, fmt.Errorf("egress identity %q has no registered NAT Gateway", egressName)
	}

	output, err := runAz("rest", "--method", "get",
		"--url", fmt.Sprintf("https://management.azure.com%s?api-version=%s", natID, apiVersion))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read NAT Gateway %s: %s", natID, string(output))
	}

	var gateway struct {
		Properties struct {
			PublicIPAddresses []struct {
				ID string `json:"id"`
			} `json:"publicIpAddresses"`
			PublicIPAddressesV6 []struct {
				ID string `json:"id"`
			} `json:"publicIpAddressesV6"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(output, &gateway); err != nil {
		return nil, nil, fmt.Errorf("failed to parse NAT Gateway %s: %w", natID, err)
	}

	lastSegment := func(id string) string {
		if i := strings.LastIndex(id, "/"); i >= 0 {
			return id[i+1:]
		}
		return id
	}
	for _, ref := range gateway.Properties.PublicIPAddresses {
		v4Names = append(v4Names, lastSegment(ref.ID))
	}
	for _, ref := range gateway.Properties.PublicIPAddressesV6 {
		v6Names = append(v6Names, lastSegment(ref.ID))
	}
	return v4Names, v6Names, nil
}

// publicIPVersionAndAddress returns the address family ARM reports for a Public IP and its
// allocated address.
func publicIPVersionAndAddress(publicIPName string) (version, address string, err error) {
	output, err := runAz("network", "public-ip", "show",
		"--resource-group", resourceGroupName,
		"--name", publicIPName,
		"--output", "json")
	if err != nil {
		return "", "", fmt.Errorf("failed to read Public IP %s: %s", publicIPName, string(output))
	}
	var pip struct {
		PublicIPAddressVersion string `json:"publicIpAddressVersion"`
		IPAddress              string `json:"ipAddress"`
	}
	if err := json.Unmarshal(output, &pip); err != nil {
		return "", "", fmt.Errorf("failed to parse Public IP %s: %w", publicIPName, err)
	}
	return pip.PublicIPAddressVersion, pip.IPAddress, nil
}

// podRegisteredAddressCount returns how many address entries the named pod contributes to its
// egress identity: one per assigned IP family. countRegisteredEndpoints counts ADDRESSES, not
// pods, so a dual-stack pod contributes 2 - asserting a pod count instead makes the wait time out
// on a dual-stack cluster before the spec reaches what it actually means to verify.
func podRegisteredAddressCount(cs clientset.Interface, namespace, podName string) int {
	ready, err := cs.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return len(ready.Status.PodIPs)
}

// dualStackAttachmentErr asserts the exact ARM shape of a dual-stack egress NAT Gateway: exactly
// one IPv4 address on publicIpAddresses and exactly one IPv6 address on publicIpAddressesV6.
//
// The exact lists matter. An IPv6 address attached to the V4 property gives the gateway no IPv6
// public path at all, and outbound has no update path, so that mistake is permanent.
func dualStackAttachmentErr(egressName string) error {
	v4Refs, v6Refs, err := natGatewayPublicIPRefs(egressName)
	if err != nil {
		return err
	}
	wantV4, wantV6 := egressName+"-pip", egressName+"-pip-v6"
	if len(v4Refs) != 1 || !strings.EqualFold(v4Refs[0], wantV4) {
		return fmt.Errorf("publicIpAddresses = %v, want [%s]", v4Refs, wantV4)
	}
	if len(v6Refs) != 1 || !strings.EqualFold(v6Refs[0], wantV6) {
		return fmt.Errorf("publicIpAddressesV6 = %v, want [%s]", v6Refs, wantV6)
	}
	return nil
}

// clusterHasIPv6Node reports whether any node exposes an IPv6 InternalIP. The provider decides
// egress families from exactly this signal, so it also decides which specs below are meaningful.
func clusterHasIPv6Node(cs clientset.Interface) bool {
	nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for i := range nodes.Items {
		for _, addr := range nodes.Items[i].Status.Addresses {
			if addr.Type != v1.NodeInternalIP {
				continue
			}
			if parsed := net.ParseIP(addr.Address); parsed != nil && parsed.To4() == nil {
				return true
			}
		}
	}
	return false
}

var _ = Describe("SLB - Dual-stack egress", Label(slbTestLabel, "SLB-DualStackEgress"), func() {
	basename := "slb-dualstack-egress"

	const (
		targetPort = 8080
		waitTime   = 5 * time.Minute
	)

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
		}
		cs = nil
		ns = nil
	})

	newEgressPod := func(name, egressName string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns.Name,
				Labels: map[string]string{egressLabel: egressName},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}},
			},
		}
	}

	// createEgressPod creates the pod, waits for it to be Ready, and returns it re-read from the
	// API so Status.PodIPs is populated. The object returned by Create has no addresses yet, and
	// callers need the assigned families to know how many addresses the identity must register.
	createEgressPod := func(name, egressName string) *v1.Pod {
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), newEgressPod(name, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ready, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ready
	}

	// registeredAddressCount is how many address entries a set of pods must contribute to an egress
	// identity. countRegisteredEndpoints counts ADDRESSES, not pods, and a dual-stack pod registers
	// one per family - so passing a pod count here silently asserts the wrong number on a
	// dual-stack cluster and the wait times out before the spec reaches what it means to verify.
	registeredAddressCount := func(pods ...*v1.Pod) int {
		total := 0
		for _, p := range pods {
			total += len(p.Status.PodIPs)
		}
		return total
	}

	requireIPv6Cluster := func() {
		if !clusterHasIPv6Node(cs) {
			Skip("no node exposes an IPv6 InternalIP; the provider provisions IPv4-only egress there")
		}
	}

	// ------------------------------------------------------------------------------------------
	// Steady state
	// ------------------------------------------------------------------------------------------

	It("attaches an IPv4 and an IPv6 Public IP, each on its own NAT Gateway property", func() {
		requireIPv6Cluster()
		const egressName = "egress-ds-shape"

		By("Creating an egress pod")
		pod := createEgressPod("ds-shape-pod", egressName)
		eventuallyEgressRegistered(egressName, registeredAddressCount(pod), waitTime)

		By("Verifying the exact ARM attachment")
		Eventually(func() error {
			return dualStackAttachmentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())

		By("Verifying each Public IP carries its declared family and an allocated address")
		v4Version, v4Addr, err := publicIPVersionAndAddress(egressName + "-pip")
		Expect(err).NotTo(HaveOccurred())
		Expect(v4Version).To(Equal("IPv4"))
		Expect(net.ParseIP(v4Addr)).NotTo(BeNil(), "the IPv4 address must actually be allocated")
		Expect(net.ParseIP(v4Addr).To4()).NotTo(BeNil(), "the V4 address must be a v4 address")

		v6Version, v6Addr, err := publicIPVersionAndAddress(egressName + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())
		Expect(v6Version).To(Equal("IPv6"),
			"an IPv4 address on the V6 list carries no IPv6 traffic")
		Expect(net.ParseIP(v6Addr)).NotTo(BeNil(), "the IPv6 address must actually be allocated")
		Expect(net.ParseIP(v6Addr).To4()).To(BeNil(), "the V6 address must be a v6 address")

		By("Deleting the egress pod and verifying BOTH addresses are reclaimed")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed(),
			"each teardown that misses the IPv6 address leaks a billable Public IP")
	})

	It("gives an IPv4-only pod a dual-stack gateway, because families follow the cluster", func() {
		requireIPv6Cluster()
		const egressName = "egress-ds-v4pod"

		By("Creating an egress pod and checking whether it got a single-stack address")
		pod := createEgressPod("ds-v4pod", egressName)
		ready, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if len(ready.Status.PodIPs) != 1 {
			Skip(fmt.Sprintf("pod is not single-stack (PodIPs=%v); this spec pins the single-stack-pod case", ready.Status.PodIPs))
		}
		eventuallyEgressRegistered(egressName, 1, waitTime)

		By("Verifying the gateway is still dual-stack")
		// A dual-stack cluster can run single-stack pods, and outbound has no update path, so the
		// families cannot be derived from whichever pod happened to create the identity: a later
		// IPv6 pod on the same identity would have no egress at all.
		Eventually(func() error {
			return dualStackAttachmentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	It("keeps two egress identities on independent address pairs", func() {
		requireIPv6Cluster()
		const egressA, egressB = "egress-ds-a", "egress-ds-b"

		By("Creating one pod per identity")
		podA := createEgressPod("ds-pod-a", egressA)
		podB := createEgressPod("ds-pod-b", egressB)
		eventuallyEgressRegistered(egressA, registeredAddressCount(podA), waitTime)
		eventuallyEgressRegistered(egressB, registeredAddressCount(podB), waitTime)

		Eventually(func() error { return dualStackAttachmentErr(egressA) }, waitTime, defaultPollInterval).Should(Succeed())
		Eventually(func() error { return dualStackAttachmentErr(egressB) }, waitTime, defaultPollInterval).Should(Succeed())

		By("Verifying the two identities do not share an address")
		_, aV6, err := publicIPVersionAndAddress(egressA + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())
		_, bV6, err := publicIPVersionAndAddress(egressB + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())
		Expect(aV6).NotTo(Equal(bV6), "each identity must own its own IPv6 address")

		By("Deleting only the first identity and verifying the second is untouched")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podA.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressA)
		}, waitTime, defaultPollInterval).Should(Succeed())
		// The surviving identity must still be fully intact: a teardown that deletes by a name
		// pattern rather than by ownership would take the other identity's addresses with it.
		Expect(dualStackAttachmentErr(egressB)).To(Succeed(),
			"deleting one egress identity must not disturb another")

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podB.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressB)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	It("recreates both addresses after the identity is scaled to zero and back", func() {
		requireIPv6Cluster()
		const egressName = "egress-ds-recycle"

		By("Creating the identity")
		pod := createEgressPod("ds-recycle-1", egressName)
		eventuallyEgressRegistered(egressName, registeredAddressCount(pod), waitTime)
		Eventually(func() error { return dualStackAttachmentErr(egressName) }, waitTime, defaultPollInterval).Should(Succeed())

		By("Removing the last pod")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())

		By("Re-creating the identity and verifying it comes back dual-stack")
		// The second create is the interesting one: it runs against a NAT Gateway name that has
		// just been deleted, and a create that reused stale state would come back single-stack.
		pod2 := createEgressPod("ds-recycle-2", egressName)
		eventuallyEgressRegistered(egressName, registeredAddressCount(pod2), waitTime)
		Eventually(func() error {
			return dualStackAttachmentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod2.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	// ------------------------------------------------------------------------------------------
	// Negative control: an IPv4-only cluster must not be charged for an unused address
	// ------------------------------------------------------------------------------------------

	It("provisions no IPv6 address on an IPv4-only cluster", func() {
		if clusterHasIPv6Node(cs) {
			Skip("cluster exposes IPv6 node addresses; this spec pins the IPv4-only topology")
		}
		const egressName = "egress-ds-v4only"

		By("Creating an egress pod")
		pod := createEgressPod("ds-v4only-pod", egressName)
		eventuallyEgressRegistered(egressName, 1, waitTime)

		By("Verifying the gateway has an IPv4 address and no IPv6 address")
		Eventually(func() error {
			v4Refs, v6Refs, err := natGatewayPublicIPRefs(egressName)
			if err != nil {
				return err
			}
			if len(v4Refs) != 1 {
				return fmt.Errorf("publicIpAddresses = %v, want exactly one", v4Refs)
			}
			if len(v6Refs) != 0 {
				return fmt.Errorf("publicIpAddressesV6 = %v, want none on an IPv4-only cluster", v6Refs)
			}
			return nil
		}, waitTime, defaultPollInterval).Should(Succeed())

		By("Verifying no IPv6 Public IP resource was created at all")
		Expect(azurePublicIPNamedAbsentErr(egressName+"-pip-v6")).To(Succeed(),
			"an IPv4-only cluster must not be billed for an IPv6 address")

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	// ------------------------------------------------------------------------------------------
	// Dataplane: the only assertion that proves the feature actually works
	// ------------------------------------------------------------------------------------------

	It("SNATs IPv6 egress to the gateway's IPv6 Public IP", func() {
		requireIPv6Cluster()
		const egressName = "egress-ds-snat"

		By("Creating an egress pod")
		pod := createEgressPod("ds-snat-pod", egressName)
		ready, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		hasV6 := false
		for _, p := range ready.Status.PodIPs {
			if parsed := net.ParseIP(p.IP); parsed != nil && parsed.To4() == nil {
				hasV6 = true
			}
		}
		if !hasV6 {
			Skip(fmt.Sprintf("pod has no IPv6 address (PodIPs=%v); IPv6 SNAT cannot be observed", ready.Status.PodIPs))
		}
		eventuallyEgressRegistered(egressName, len(ready.Status.PodIPs), waitTime)
		Eventually(func() error { return dualStackAttachmentErr(egressName) }, waitTime, defaultPollInterval).Should(Succeed())

		_, wantV6Addr, err := publicIPVersionAndAddress(egressName + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())
		Expect(wantV6Addr).NotTo(BeEmpty())

		By("Observing the source address of IPv6 egress from the pod")
		// This is the only assertion in the suite that proves NRP actually routes IPv6 egress
		// through the address we attached. Everything else verifies the request we sent.
		var observed string
		Eventually(func() string {
			out, _ := utils.RunKubectl(ns.Name, "exec", pod.Name, "--",
				"/bin/sh", "-c", "curl -s -m 10 -6 https://ifconfig.co/ip || true")
			observed = strings.TrimSpace(ipv6SourceRegexp.FindString(out))
			return observed
		}, waitTime, defaultPollInterval).ShouldNot(BeEmpty(),
			"no IPv6 source address was observable; IPv6 egress is not reaching the internet at all")

		Expect(net.ParseIP(observed)).NotTo(BeNil())
		Expect(net.ParseIP(observed).Equal(net.ParseIP(wantV6Addr))).To(BeTrue(),
			"IPv6 egress left as %s but the gateway's IPv6 Public IP is %s: traffic is bypassing the NAT Gateway, so IPv6 SNAT is not in effect",
			observed, wantV6Addr)

		utils.Logf("  ✓ IPv6 egress SNAT verified as NAT gateway IPv6 public IP %s", observed)

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})
})

// ----------------------------------------------------------------------------------------------
// Restart and initialization. These need the CCM cluster, so they live in their own Describe with
// their own BeforeEach gate.
// ----------------------------------------------------------------------------------------------

var _ = Describe("SLB - Dual-stack egress across CCM restart", Label(slbTestLabel, "SLB-DualStackEgress", "SLB-CCMRestart"), func() {
	basename := "slb-dualstack-restart"

	const (
		targetPort = 8080
		waitTime   = 5 * time.Minute
	)

	var (
		cs        clientset.Interface
		ccmClient *CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error
		if !IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM restart tests: %s environment variable not set", CCMKubeconfigEnvVar))
		}
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())
		ccmClient, err = NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		if !clusterHasIPv6Node(cs) {
			Skip("no node exposes an IPv6 InternalIP; the provider provisions IPv4-only egress there")
		}
	})

	AfterEach(func() {
		if ccmClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), CCMRecoveryTimeout)
			_ = ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout)
			cancel()
		}
		if cs != nil && ns != nil {
			Expect(utils.DeleteNamespace(cs, ns.Name)).To(Succeed())
		}
		cs = nil
		ns = nil
		ccmClient = nil
	})

	egressPod := func(name, egressName string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns.Name,
				Labels: map[string]string{egressLabel: egressName},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}},
			},
		}
	}

	It("creates a dual-stack gateway for an identity that first appears while CCM is down", func() {
		const egressName = "egress-ds-coldstart"

		By("Stopping CCM")
		ctx, cancel := context.WithTimeout(context.Background(), CCMRecoveryTimeout)
		defer cancel()
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Creating the egress pod while nothing is watching")
		// The identity is therefore provisioned by the startup reconcile path rather than the pod
		// event path. Those are two different call sites, and a create that skipped the family
		// decision on the startup path would produce a permanently IPv4-only gateway: outbound has
		// no update path, so no later reconcile repairs it.
		_, err = cs.CoreV1().Pods(ns.Name).Create(context.TODO(), egressPod("ds-cold-pod", egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Restarting CCM")
		Expect(ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)).To(Succeed())

		By("Verifying the gateway created at startup is dual-stack")
		eventuallyEgressRegistered(egressName, podRegisteredAddressCount(cs, ns.Name, "ds-cold-pod"), waitTime)
		Eventually(func() error {
			return dualStackAttachmentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed(),
			"an identity provisioned by startup reconciliation must get the same families as one provisioned by a pod event")

		By("Cleaning up")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), "ds-cold-pod", metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	It("leaves an existing dual-stack identity's addresses untouched across a restart", func() {
		const egressName = "egress-ds-survive"

		By("Creating the identity while CCM is running")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), egressPod("ds-survive-pod", egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyEgressRegistered(egressName, podRegisteredAddressCount(cs, ns.Name, "ds-survive-pod"), waitTime)
		Eventually(func() error { return dualStackAttachmentErr(egressName) }, waitTime, defaultPollInterval).Should(Succeed())

		_, beforeV4, err := publicIPVersionAndAddress(egressName + "-pip")
		Expect(err).NotTo(HaveOccurred())
		_, beforeV6, err := publicIPVersionAndAddress(egressName + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())

		By("Restarting CCM")
		ctx, cancel := context.WithTimeout(context.Background(), CCMRecoveryTimeout)
		defer cancel()
		Expect(ccmClient.CrashCCMAndWaitForRecovery(ctx, CCMRecoveryTimeout)).To(Succeed())

		By("Verifying startup did not treat the IPv6 address as an orphan")
		// Startup orphan cleanup walks every Public IP in the resource group. The IPv6 egress
		// address is a name it has never owned before this feature, so a scan that mis-classifies
		// it would delete a live address and silently break IPv6 egress.
		Consistently(func() error {
			return dualStackAttachmentErr(egressName)
		}, 90*time.Second, defaultPollInterval).Should(Succeed(),
			"a restart must not detach or delete either address of a live egress identity")

		_, afterV4, err := publicIPVersionAndAddress(egressName + "-pip")
		Expect(err).NotTo(HaveOccurred())
		_, afterV6, err := publicIPVersionAndAddress(egressName + "-pip-v6")
		Expect(err).NotTo(HaveOccurred())
		Expect(afterV4).To(Equal(beforeV4), "the IPv4 address must survive a restart unchanged")
		Expect(afterV6).To(Equal(beforeV6), "the IPv6 address must survive a restart unchanged")

		By("Cleaning up")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), "ds-survive-pod", metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed())
	})

	It("reclaims both addresses for an identity deleted while CCM is down", func() {
		const egressName = "egress-ds-colddelete"

		By("Creating the identity while CCM is running")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), egressPod("ds-colddel-pod", egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyEgressRegistered(egressName, podRegisteredAddressCount(cs, ns.Name, "ds-colddel-pod"), waitTime)
		Eventually(func() error { return dualStackAttachmentErr(egressName) }, waitTime, defaultPollInterval).Should(Succeed())

		By("Stopping CCM and removing the last pod while nothing is watching")
		ctx, cancel := context.WithTimeout(context.Background(), CCMRecoveryTimeout)
		defer cancel()
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), "ds-colddel-pod", metav1.DeleteOptions{})).To(Succeed())

		By("Restarting CCM")
		Expect(ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)).To(Succeed())

		By("Verifying startup teardown reclaimed BOTH addresses")
		// The deletion is discovered by the startup diff, not by a pod event. A teardown that only
		// knows the IPv4 name leaves the IPv6 address behind with no NAT Gateway to anchor it, and
		// nothing ever reclaims it.
		Eventually(func() error {
			return azureEgressResourcesAbsentErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed(),
			"an identity removed while CCM was down must not leak its IPv6 address")
	})
})
