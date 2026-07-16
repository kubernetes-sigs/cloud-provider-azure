/*
Copyright 2025 The Kubernetes Authors.

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	utilnet "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// egressOutboundGoneErr returns nil once no Outbound service with the given name remains in the
// Service Gateway (i.e. the NAT gateway/outbound registration has been torn down).
func egressOutboundGoneErr(egressName string) error {
	resp, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	for _, s := range resp.Value {
		if s.Properties.ServiceType == "Outbound" && s.Name == egressName {
			return fmt.Errorf("outbound service %s still registered", egressName)
		}
	}
	return nil
}

// Egress (NAT gateway) lifecycle edge case: draining the last egress pod must tear the NAT
// gateway down, and re-adding pods must rebuild it.
var _ = Describe("SLB - Egress Lifecycle", Label(slbTestLabel), func() {
	basename := "slb-egress-lifecycle-test"

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
			By("Waiting for Azure cleanup (egress cleanup is slower)")
			eventuallyAzureCleanup(3 * time.Minute)
			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()
			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	makeEgressPod := func(name, egressName string, targetPort int) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: map[string]string{egressLabel: egressName}},
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

	It("should tear down the NAT gateway when the last egress pod is removed and rebuild it when pods return", func() {
		const (
			numPods    = 2
			egressName = "egress-drain-gateway"
			targetPort = 8080
			waitTime   = 2 * time.Minute
		)
		egressSelector := egressLabel + "=" + egressName

		By(fmt.Sprintf("Creating %d egress pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(fmt.Sprintf("egress-pod-%d", i), egressName, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for the NAT gateway to be provisioned and pods registered")
		eventuallyEgressRegistered(egressName, numPods, waitTime)

		By("Deleting all egress pods")
		Expect(cs.CoreV1().Pods(ns.Name).DeleteCollection(context.TODO(), metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: egressSelector})).To(Succeed())

		By("Verifying the egress pods drain (their finalizers clear after NAT gateway teardown)")
		Eventually(func() (int, error) {
			pods, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{LabelSelector: egressSelector})
			if err != nil {
				return -1, err
			}
			return len(pods.Items), nil
		}, waitTime, defaultPollInterval).Should(Equal(0), "egress pods should fully delete")

		By("Verifying the outbound service is removed from the Service Gateway")
		Eventually(func() error {
			return egressOutboundGoneErr(egressName)
		}, waitTime, defaultPollInterval).Should(Succeed(), "the NAT gateway/outbound service should be torn down")

		By(fmt.Sprintf("Recreating %d egress pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(fmt.Sprintf("egress-pod-new-%d", i), egressName, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Verifying the NAT gateway is rebuilt and pods re-registered")
		eventuallyEgressRegistered(egressName, numPods, waitTime)

		utils.Logf("\n✓ Egress NAT gateway drained to zero and rebuilt")
	})
})

// Inbound idempotency under rapid create/delete of the SAME service name: each cycle must
// provision a fresh LB under a new UID and tear it down cleanly without stranding finalizers.
var _ = Describe("SLB - Service Idempotency", Label(slbTestLabel), func() {
	basename := "slb-idempotency-test"

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

	It("should cleanly provision and tear down across rapid same-name create/delete cycles", func() {
		const (
			cycles      = 3
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "recycled-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d backing pods once", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", serviceName, i), Namespace: ns.Name, Labels: labels},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "test-app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}

		var lastUID string
		for c := 1; c <= cycles; c++ {
			By(fmt.Sprintf("Cycle %d/%d: creating service %s", c, cycles, serviceName))
			created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			uid := string(created.UID)
			Expect(uid).NotTo(Equal(lastUID), "each recreate must get a fresh UID")
			lastUID = uid

			By(fmt.Sprintf("Cycle %d/%d: waiting for provisioning + registration", c, cycles))
			eventuallyServiceReconciled(uid, numPods, waitTime)

			By(fmt.Sprintf("Cycle %d/%d: deleting service and waiting for full teardown", c, cycles))
			Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() error {
				// Wait for the K8s object to fully disappear (finalizers cleared) AND the SGW
				// entry to deregister, so the next cycle's create cannot race a pending delete.
				if _, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{}); getErr == nil {
					return fmt.Errorf("service %s still exists (deletion in progress)", serviceName)
				}
				return serviceDeletedErr(uid)
			}, waitTime, defaultPollInterval).Should(Succeed(),
				"service should fully delete and deregister before the next cycle")
		}

		utils.Logf("\n✓ %d same-name create/delete cycles completed cleanly", cycles)
	})
})

// Dual-stack egress: a pod that receives one IP per family (Status.PodIPs) must have BOTH families
// registered under the NAT gateway, drained on delete, and its single finalizer released only after
// every family has left NRP. Skips cleanly on a single-stack cluster.
var _ = Describe("SLB - Dual-Stack Egress", Label(slbTestLabel), func() {
	basename := "slb-dualstack-egress-test"

	const targetPort = 8080

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
			By("Waiting for Azure cleanup (egress cleanup is slower)")
			eventuallyAzureCleanup(3 * time.Minute)
			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()
			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	makeEgressPod := func(name, egressName string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: map[string]string{egressLabel: egressName}},
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

	// dualStackPodIPs returns a ready pod's IPs, skipping the test on a single-stack cluster (a pod
	// that receives fewer than two IP families cannot exercise dual-stack egress).
	dualStackPodIPs := func(name string) []string {
		ready, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if len(ready.Status.PodIPs) < 2 {
			Skip(fmt.Sprintf("cluster does not assign dual-stack pod IPs (PodIPs=%v)", ready.Status.PodIPs))
		}
		ips := make([]string, 0, len(ready.Status.PodIPs))
		for _, p := range ready.Status.PodIPs {
			ips = append(ips, p.IP)
		}
		return ips
	}

	// egressAddressRegistered reports whether podIP is registered as an egress address for egressName.
	egressAddressRegistered := func(egressName, podIP string) bool {
		resp, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())
		for _, loc := range resp.Value {
			for _, addr := range loc.Addresses {
				if !ipEqual(addr.Address, podIP) {
					continue
				}
				for _, svc := range addr.Services {
					if svc == egressName {
						return true
					}
				}
			}
		}
		return false
	}

	podGone := func(name string) bool {
		_, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}

	It("registers every IP family of a dual-stack egress pod under the NAT gateway", func() {
		const egressName, podName = "egress-ds-register", "egress-ds-register-pod"
		const waitTime = 2 * time.Minute

		By("Creating a dual-stack egress pod")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)

		By("Waiting for the NAT gateway and every pod IP family to register")
		// countRegisteredEndpoints counts address entries; a dual-stack pod contributes one per family,
		// so a single pod must register len(PodIPs) addresses (before this fix it would have been 1).
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Verifying each specific pod IP family is registered under the NAT gateway")
		for _, ip := range ips {
			Eventually(func() bool { return egressAddressRegistered(egressName, ip) }, waitTime, defaultPollInterval).
				Should(BeTrue(), "pod IP %s must be registered under egress %s", ip, egressName)
		}

		By("Verifying no address location mixes IP families")
		assertAddressLocationFamilyPurity()
		utils.Logf("\n✓ Registered %d dual-stack egress addresses: %v", len(ips), ips)
	})

	It("drains every IP family and releases the finalizer when a dual-stack egress pod is deleted", func() {
		const egressName = "egress-ds-drain"
		const delName, keepName = "egress-ds-drain-pod", "egress-ds-keep-pod"
		const waitTime = 3 * time.Minute

		By("Creating two dual-stack egress pods sharing one NAT gateway")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(delName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(keepName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		delIPs := dualStackPodIPs(delName)
		keepIPs := dualStackPodIPs(keepName)

		By("Waiting for both pods' families to register")
		eventuallyEgressRegistered(egressName, len(delIPs)+len(keepIPs), waitTime)

		By("Deleting one dual-stack pod")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), delName, metav1.DeleteOptions{})).To(Succeed())

		By("Verifying the deleted pod fully terminates (its finalizer clears only after every address drains)")
		Eventually(func() bool { return podGone(delName) }, waitTime, defaultPollInterval).
			Should(BeTrue(), "the deleted dual-stack pod should fully delete once every family has drained")

		By("Verifying every family of the deleted pod is drained from the NAT gateway")
		for _, ip := range delIPs {
			Eventually(func() bool { return egressAddressRegistered(egressName, ip) }, waitTime, defaultPollInterval).
				Should(BeFalse(), "deleted pod IP %s must be drained from egress %s", ip, egressName)
		}

		By("Verifying the surviving pod's families remain registered")
		for _, ip := range keepIPs {
			Expect(egressAddressRegistered(egressName, ip)).To(BeTrue(), "surviving pod IP %s must remain registered", ip)
		}
		utils.Logf("\n✓ Deleted pod drained %v; survivor kept %v", delIPs, keepIPs)
	})

	It("tears down the NAT gateway after the last dual-stack egress pod is removed", func() {
		const egressName, podName = "egress-ds-lastpod", "egress-ds-lastpod-pod"
		const waitTime = 3 * time.Minute

		By("Creating a single dual-stack egress pod")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)

		By("Waiting for the NAT gateway with every family registered")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Deleting the sole dual-stack egress pod")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})).To(Succeed())

		By("Verifying the pod fully terminates")
		Eventually(func() bool { return podGone(podName) }, waitTime, defaultPollInterval).
			Should(BeTrue(), "the last dual-stack egress pod should fully delete")

		By("Verifying the NAT gateway/outbound service is torn down")
		Eventually(func() error { return egressOutboundGoneErr(egressName) }, waitTime, defaultPollInterval).
			Should(Succeed(), "the NAT gateway should be torn down after the last dual-stack pod")
		utils.Logf("\n✓ NAT gateway torn down after last dual-stack pod (families %v)", ips)
	})

	It("files every IP family under its own-family node location, never a mixed-family location", func() {
		const egressName, podName = "egress-ds-purity", "egress-ds-purity-pod"
		const waitTime = 2 * time.Minute

		By("Creating a dual-stack egress pod")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)

		By("Waiting for every pod IP family to register")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)
		for _, ip := range ips {
			Eventually(func() bool { return egressAddressRegistered(egressName, ip) }, waitTime, defaultPollInterval).
				Should(BeTrue(), "pod IP %s must be registered under egress %s", ip, egressName)
		}

		By("Asserting no address location mixes IP families (the exact payload NRP rejects)")
		assertAddressLocationFamilyPurity()

		By("Asserting the pod's two families occupy two DISTINCT family-matched node locations")
		v4Loc := egressLocationForAddress(egressName, podIPOfFamily2(ips, false))
		v6Loc := egressLocationForAddress(egressName, podIPOfFamily2(ips, true))
		Expect(v4Loc).NotTo(BeEmpty(), "the IPv4 pod address must be registered under a node location")
		Expect(v6Loc).NotTo(BeEmpty(), "the IPv6 pod address must be registered under a node location")
		Expect(utilnet.IsIPv6String(v4Loc)).To(BeFalse(), "the IPv4 pod address must sit under an IPv4 node location, got %s", v4Loc)
		Expect(utilnet.IsIPv6String(v6Loc)).To(BeTrue(), "the IPv6 pod address must sit under an IPv6 node location, got %s", v6Loc)
		Expect(ipEqual(v4Loc, v6Loc)).To(BeFalse(), "the two families must occupy distinct node locations, both were %s", v4Loc)
		utils.Logf("\n✓ Family-partitioned: IPv4 under %s, IPv6 under %s", v4Loc, v6Loc)
	})

	It("keeps both IP families registered continuously without dropping one", func() {
		const egressName, podName = "egress-ds-stable", "egress-ds-stable-pod"
		const waitTime = 2 * time.Minute

		By("Creating a dual-stack egress pod")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)

		By("Waiting for every pod IP family to register")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Confirming both families stay registered (a re-registration must never drop the shared family)")
		Consistently(func() bool {
			for _, ip := range ips {
				if !egressAddressRegistered(egressName, ip) {
					return false
				}
			}
			return true
		}, 45*time.Second, defaultPollInterval).
			Should(BeTrue(), "every family of a stable dual-stack egress pod must remain registered")
		utils.Logf("\n✓ Both families remained registered for the observation window: %v", ips)
	})

	It("re-seeds every family under its node location after a CCM restart", func() {
		const egressName, podName = "egress-ds-recover", "egress-ds-recover-pod"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Creating a dual-stack egress pod and waiting for both families to register")
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)
		eventuallyEgressRegistered(egressName, len(ips), waitTime)
		v4Before := egressLocationForAddress(egressName, podIPOfFamily2(ips, false))
		v6Before := egressLocationForAddress(egressName, podIPOfFamily2(ips, true))
		Expect(v4Before).NotTo(BeEmpty())
		Expect(v6Before).NotTo(BeEmpty())

		By("Restarting the cloud-controller-manager (scale to 0, then back up)")
		ccmClient, err := utils.NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		// Guarantee the CCM is restored even if the assertions below fail, so namespace/Azure cleanup
		// in AfterEach (which needs a running CCM) still works.
		DeferCleanup(func() {
			_ = scaleCCMDeployment(context.TODO(), ccmClient, 1)
			_ = waitForCCMFullyUp(context.TODO(), ccmClient, utils.CCMRecoveryTimeout)
		})
		Expect(scaleCCMDeployment(ctx, ccmClient, 0)).To(Succeed())
		Expect(waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)).To(Succeed())
		Expect(scaleCCMDeployment(ctx, ccmClient, 1)).To(Succeed())
		Expect(waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)).To(Succeed())

		By("Verifying init recovery re-seeds each family under its SAME node location (not flattened onto HostIP)")
		for _, ip := range ips {
			Eventually(func() bool { return egressAddressRegistered(egressName, ip) }, waitTime, defaultPollInterval).
				Should(BeTrue(), "family %s must remain registered after a CCM restart", ip)
		}
		assertAddressLocationFamilyPurity()
		Expect(egressLocationForAddress(egressName, podIPOfFamily2(ips, false))).To(Equal(v4Before), "the IPv4 family must recover under the same IPv4 node location")
		Expect(egressLocationForAddress(egressName, podIPOfFamily2(ips, true))).To(Equal(v6Before), "the IPv6 family must recover under the same IPv6 node location")
		utils.Logf("\n✓ CCM restart recovered both families under family-matched locations: v4=%s v6=%s", v4Before, v6Before)
	})

	It("holds the pod Terminating until every family has drained from NRP", func() {
		const egressName, podName = "egress-ds-gate", "egress-ds-gate-pod"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Creating a dual-stack egress pod and waiting for both families to register")
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Confirming the pod carries the ServiceGateway cleanup finalizer")
		live, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(hasFinalizer(live.Finalizers, serviceGatewayPodFinalizer)).To(BeTrue(),
			"a registered egress pod must carry the drain-gating cleanup finalizer")

		By("Deleting the pod")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})).To(Succeed())

		By("Asserting the pod is never reclaimed while any family is still registered in NRP")
		Consistently(func() bool {
			for _, ip := range ips {
				// Query registration first: if an address is still in NRP, the pod must still exist
				// (its single finalizer gates deletion until every family drains).
				if egressAddressRegistered(egressName, ip) && podGone(podName) {
					return false
				}
			}
			return true
		}, waitTime, defaultPollInterval).
			Should(BeTrue(), "the finalizer must hold the pod Terminating until BOTH families drain")

		By("Verifying the pod is eventually reclaimed with every family drained")
		Eventually(func() bool { return podGone(podName) }, waitTime, defaultPollInterval).
			Should(BeTrue(), "the pod must be reclaimed once every family has drained")
		for _, ip := range ips {
			Expect(egressAddressRegistered(egressName, ip)).To(BeFalse(), "family %s must be drained after the pod is reclaimed", ip)
		}
		utils.Logf("\n✓ Finalizer gated deletion until both families drained: %v", ips)
	})

	It("moves both families to the new gateway when a dual-stack egress pod's label changes", func() {
		const egressA, egressB, podName = "egress-ds-move-a", "egress-ds-move-b", "egress-ds-move-pod"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Creating a dual-stack egress pod under gateway A")
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, makeEgressPod(podName, egressA), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)
		eventuallyEgressRegistered(egressA, len(ips), waitTime)

		By("Relabelling the pod to move it from gateway A to gateway B")
		moved, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		moved.Labels[egressLabel] = egressB
		_, err = cs.CoreV1().Pods(ns.Name).Update(ctx, moved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying both families register under gateway B")
		for _, ip := range ips {
			Eventually(func() bool { return egressAddressRegistered(egressB, ip) }, waitTime, defaultPollInterval).
				Should(BeTrue(), "family %s must register under the new gateway B", ip)
		}

		By("Verifying both families drain from gateway A")
		for _, ip := range ips {
			Eventually(func() bool { return egressAddressRegistered(egressA, ip) }, waitTime, defaultPollInterval).
				Should(BeFalse(), "family %s must drain from the old gateway A", ip)
		}

		By("Asserting family purity after the move and that the pod kept its finalizer")
		assertAddressLocationFamilyPurity()
		live, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(hasFinalizer(live.Finalizers, serviceGatewayPodFinalizer)).To(BeTrue(),
			"the live pod must keep its cleanup finalizer across an egress-label move")
		utils.Logf("\n✓ Both families moved A->B under family-pure locations: %v", ips)
	})

	It("registers dual-stack egress pods from multiple nodes under per-node family-matched locations", func() {
		const egressName = "egress-ds-multinode"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Finding at least two dual-stack nodes")
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		type nodeIPs struct{ v4, v6 string }
		dsNodes := map[string]nodeIPs{}
		var names []string
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
				dsNodes[node.Name] = nodeIPs{v4, v6}
				names = append(names, node.Name)
			}
		}
		if len(names) < 2 {
			Skip(fmt.Sprintf("need at least two dual-stack nodes, found %d", len(names)))
		}
		nodeA, nodeB := names[0], names[1]

		By("Creating one dual-stack egress pod pinned to each of two nodes, sharing one gateway")
		makeOnNode := func(name, nodeName string) *v1.Pod {
			p := makeEgressPod(name, egressName)
			p.Spec.NodeName = nodeName
			return p
		}
		_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, makeOnNode("egress-ds-mn-a", nodeA), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, makeOnNode("egress-ds-mn-b", nodeB), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ipsA := dualStackPodIPs("egress-ds-mn-a")
		ipsB := dualStackPodIPs("egress-ds-mn-b")

		By("Waiting for every family of both pods to register")
		eventuallyEgressRegistered(egressName, len(ipsA)+len(ipsB), waitTime)

		By("Asserting each pod's families sit under its OWN node's family-matched locations")
		assertPodFamiliesUnderNode := func(ips []string, n nodeIPs) {
			v4Loc := egressLocationForAddress(egressName, podIPOfFamily2(ips, false))
			v6Loc := egressLocationForAddress(egressName, podIPOfFamily2(ips, true))
			Expect(ipEqual(v4Loc, n.v4)).To(BeTrue(), "IPv4 family must sit under its node IPv4 %s, got %s", n.v4, v4Loc)
			Expect(ipEqual(v6Loc, n.v6)).To(BeTrue(), "IPv6 family must sit under its node IPv6 %s, got %s", n.v6, v6Loc)
		}
		assertPodFamiliesUnderNode(ipsA, dsNodes[nodeA])
		assertPodFamiliesUnderNode(ipsB, dsNodes[nodeB])
		assertAddressLocationFamilyPurity()

		By("Deleting one node's pod and confirming only its families drain")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(ctx, "egress-ds-mn-a", metav1.DeleteOptions{})).To(Succeed())
		for _, ip := range ipsA {
			Eventually(func() bool { return egressAddressRegistered(egressName, ip) }, waitTime, defaultPollInterval).
				Should(BeFalse(), "deleted node-A pod family %s must drain", ip)
		}
		for _, ip := range ipsB {
			Expect(egressAddressRegistered(egressName, ip)).To(BeTrue(), "surviving node-B pod family %s must stay registered", ip)
		}
		utils.Logf("\n✓ Multi-node dual-stack egress: nodeA=%s families %v drained, nodeB=%s families %v kept", nodeA, ipsA, nodeB, ipsB)
	})

	It("registers a pod that is both an inbound LB backend and egress, and egress removal preserves inbound", func() {
		const egressName, podName, svcName = "egress-ds-dualrole", "egress-ds-dualrole-pod", "dualrole-lb"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()
		appLabels := map[string]string{"app": "egress-ds-dualrole"}

		By("Creating a dual-stack pod that is both egress-labelled and an LB backend")
		pod := makeEgressPod(podName, egressName)
		for k, v := range appLabels {
			pod.Labels[k] = v
		}
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              appLabels,
				Ports:                 []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(ctx, svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		lbUID := string(created.UID)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)
		primaryIP := ips[0] // the LB service uses the cluster primary family; that address carries both roles

		By("Waiting for the pod to register for both its egress and its inbound LB service")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)
		Eventually(func() error { return serviceReconciledErr(lbUID, -1) }, waitTime, defaultPollInterval).Should(Succeed())

		By("Asserting the primary-family address references BOTH the egress and the inbound service")
		Eventually(func() bool {
			svcs := addressServicesOf(primaryIP)
			return servicesContain(svcs, egressName) && servicesContain(svcs, lbUID)
		}, waitTime, defaultPollInterval).Should(BeTrue(),
			"the primary pod IP %s must reference both egress %q and inbound %q", primaryIP, egressName, lbUID)
		for _, ip := range ips {
			Expect(servicesContain(addressServicesOf(ip), egressName)).To(BeTrue(), "family %s must be egress-registered", ip)
		}
		assertAddressLocationFamilyPurity()

		By("Dropping the egress label and confirming the inbound registration survives on the shared address")
		upd, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		delete(upd.Labels, egressLabel)
		_, err = cs.CoreV1().Pods(ns.Name).Update(ctx, upd, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool { return !servicesContain(addressServicesOf(primaryIP), egressName) }, waitTime, defaultPollInterval).
			Should(BeTrue(), "the egress reference for %s must be removed after the egress label is dropped", primaryIP)
		Expect(servicesContain(addressServicesOf(primaryIP), lbUID)).To(BeTrue(),
			"the inbound LB reference for %s must survive the egress removal (identities are isolated)", primaryIP)
		assertAddressLocationFamilyPurity()

		By("Cleaning up the LB service")
		Expect(cs.CoreV1().Services(ns.Name).Delete(ctx, svcName, metav1.DeleteOptions{})).To(Succeed())
		utils.Logf("\n✓ Dual-role pod: egress+inbound coexisted; egress removal preserved inbound on %s", primaryIP)
	})

	It("keeps address locations family-pure and leak-free under pod scale churn", func() {
		const egressName = "egress-ds-churn"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		create := func(names []string) {
			for _, n := range names {
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, makeEgressPod(n, egressName), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		}
		delAndWait := func(names []string) {
			for _, n := range names {
				Expect(cs.CoreV1().Pods(ns.Name).Delete(ctx, n, metav1.DeleteOptions{})).To(Succeed())
			}
			for _, n := range names {
				Eventually(func() bool { return podGone(n) }, waitTime, defaultPollInterval).
					Should(BeTrue(), "churned pod %s must fully terminate (no stuck finalizer)", n)
			}
		}
		countIPs := func(names []string) int {
			total := 0
			for _, n := range names {
				total += len(dualStackPodIPs(n))
			}
			return total
		}

		batch1 := []string{"egress-ds-churn-a", "egress-ds-churn-b", "egress-ds-churn-c"}
		batch2 := []string{"egress-ds-churn-d", "egress-ds-churn-e", "egress-ds-churn-f"}

		By("Creating the first batch and asserting family purity")
		create(batch1)
		eventuallyEgressRegistered(egressName, countIPs(batch1), waitTime)
		assertAddressLocationFamilyPurity()

		By("Churning: add a second batch, remove the first, and re-assert purity")
		create(batch2)
		want := countIPs(batch2)
		delAndWait(batch1)
		eventuallyEgressRegistered(egressName, want, waitTime)
		assertAddressLocationFamilyPurity()

		By("Removing the remaining batch and confirming no stuck finalizers")
		delAndWait(batch2)
		utils.Logf("\n✓ Family purity and clean termination held across scale churn")
	})

	It("registers a hostNetwork dual-stack egress pod (PodIPs equal the node IPs) family-matched", func() {
		const egressName, podName = "egress-ds-hostnet", "egress-ds-hostnet-pod"
		const hostPort = 39180
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Creating a hostNetwork egress pod")
		pod := makeEgressPod(podName, egressName)
		pod.Spec.HostNetwork = true
		pod.Spec.Containers[0].Args = []string{"netexec", fmt.Sprintf("--http-port=%d", hostPort)}
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)

		By("Waiting for both families (which equal the node IPs) to register")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Asserting each family registers under its own-family node location (address equals location)")
		for _, ip := range ips {
			loc := egressLocationForAddress(egressName, ip)
			Expect(loc).NotTo(BeEmpty(), "hostNetwork pod IP %s must be registered", ip)
			Expect(utilnet.IsIPv6String(loc)).To(Equal(utilnet.IsIPv6String(ip)),
				"hostNetwork address %s must sit under a same-family location, got %s", ip, loc)
		}
		assertAddressLocationFamilyPurity()
		utils.Logf("\n✓ hostNetwork dual-stack egress registered family-matched: %v", ips)
	})

	It("still gates deletion via the finalizer under a force-delete (grace period 0)", func() {
		const egressName, podName = "egress-ds-force", "egress-ds-force-pod"
		const waitTime = 3 * time.Minute
		ctx := context.TODO()

		By("Creating a dual-stack egress pod and waiting for both families to register")
		_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, makeEgressPod(podName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		ips := dualStackPodIPs(podName)
		eventuallyEgressRegistered(egressName, len(ips), waitTime)

		By("Force-deleting the pod (grace period 0)")
		zero := int64(0)
		Expect(cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})).To(Succeed())

		By("Asserting the finalizer still gates deletion until every family drains (a force-delete must not bypass it)")
		Consistently(func() bool {
			for _, ip := range ips {
				if egressAddressRegistered(egressName, ip) && podGone(podName) {
					return false
				}
			}
			return true
		}, waitTime, defaultPollInterval).
			Should(BeTrue(), "a force-delete must not reclaim the pod while a family is still registered in NRP")

		By("Verifying the pod is eventually reclaimed with every family drained")
		Eventually(func() bool { return podGone(podName) }, waitTime, defaultPollInterval).
			Should(BeTrue(), "the pod must be reclaimed once every family drains")
		for _, ip := range ips {
			Expect(egressAddressRegistered(egressName, ip)).To(BeFalse(), "family %s must be drained", ip)
		}
		utils.Logf("\n✓ Force-delete honored the drain-gate: %v", ips)
	})
})

// assertAddressLocationFamilyPurity fails if any address location holds an address of a different IP
// family than the location's node IP. NRP rejects a mixed-family location
// (IPv4LocationCannotContainIPv6Addresses), so every address must sit under a same-family location.
func assertAddressLocationFamilyPurity() {
	resp, err := queryServiceGatewayAddressLocations()
	Expect(err).NotTo(HaveOccurred())
	for _, loc := range resp.Value {
		locIsV6 := utilnet.IsIPv6String(loc.AddressLocation)
		for _, addr := range loc.Addresses {
			Expect(utilnet.IsIPv6String(addr.Address)).To(Equal(locIsV6),
				"address %s (services=%v) is filed under the %s node location %s; NRP rejects mixed-family locations",
				addr.Address, addr.Services, ipFamilyName(locIsV6), loc.AddressLocation)
		}
	}
}

// egressLocationForAddress returns the node location (address location key) under which podIP is
// registered for egressName, or "" if it is not registered.
func egressLocationForAddress(egressName, podIP string) string {
	if podIP == "" {
		return ""
	}
	resp, err := queryServiceGatewayAddressLocations()
	Expect(err).NotTo(HaveOccurred())
	for _, loc := range resp.Value {
		for _, addr := range loc.Addresses {
			if !ipEqual(addr.Address, podIP) {
				continue
			}
			for _, svc := range addr.Services {
				if svc == egressName {
					return loc.AddressLocation
				}
			}
		}
	}
	return ""
}

// podIPOfFamily2 returns the first address of the requested family (ipv6=true for IPv6) from a list
// of pod IP strings, or "" when none is present.
func podIPOfFamily2(ips []string, ipv6 bool) string {
	for _, ip := range ips {
		if utilnet.IsIPv6String(ip) == ipv6 {
			return ip
		}
	}
	return ""
}

// addressServicesOf returns the Service Gateway service IDs that reference podIP across all address
// locations (both inbound LoadBalancer UIDs and outbound egress names), or nil if it is not
// registered.
func addressServicesOf(podIP string) []string {
	if podIP == "" {
		return nil
	}
	resp, err := queryServiceGatewayAddressLocations()
	Expect(err).NotTo(HaveOccurred())
	for _, loc := range resp.Value {
		for _, addr := range loc.Addresses {
			if ipEqual(addr.Address, podIP) {
				return addr.Services
			}
		}
	}
	return nil
}

// servicesContain reports whether services includes id.
func servicesContain(services []string, id string) bool {
	for _, s := range services {
		if s == id {
			return true
		}
	}
	return false
}

// ipFamilyName returns a human label for an IP family.
func ipFamilyName(isV6 bool) string {
	if isV6 {
		return "IPv6"
	}
	return "IPv4"
}
