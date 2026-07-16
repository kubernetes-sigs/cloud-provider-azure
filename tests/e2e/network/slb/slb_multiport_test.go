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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// These specs cover multi-port Container Load Balancer services:
//   - a service with several ports mapped to DISTINCT backend ports must produce one LB
//     rule per port; and
//   - a service whose ports resolve to the SAME protocol + backend port must be rejected,
//     because Azure refuses two load-balancing rules that share a backend port and protocol
//     on the same pool with floating IP disabled (RulesUseSameBackendPortProtocolAndPool).
//     The difftracker detects this at build time and terminally parks the service, so it
//     must never provision any Azure resources.
var _ = Describe("SLB - Multi-Port Service", Label(slbTestLabel), func() {
	basename := "slb-multiport-test"

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
			eventuallyAzureCleanup(3 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	// createReadyAgnhostPods creates numPods agnhost pods that serve HTTP on listenPort and
	// become Ready via a readiness probe on that port, so they register as endpoints.
	createReadyAgnhostPods := func(serviceName string, labels map[string]string, numPods, listenPort int) {
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "test-app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", listenPort)},
						ReadinessProbe: &v1.Probe{
							ProbeHandler: v1.ProbeHandler{
								HTTPGet: &v1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt(listenPort),
								},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       2,
						},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
	}

	It("should create one LB rule per port for a multi-port service with distinct backend ports", func() {
		const (
			serviceName = "multiport-distinct"
			numPods     = 3
			listenPort  = 8080
			portA       = int32(80)
			portB       = int32(443)
			targetA     = 8080
			targetB     = 8443
		)
		labels := map[string]string{"app": serviceName}

		By("Creating backend pods")
		createReadyAgnhostPods(serviceName, labels, numPods, listenPort)

		By("Creating a LoadBalancer service with two ports mapped to distinct backend ports")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					{Name: "http", Port: portA, TargetPort: intstr.FromInt(targetA), Protocol: v1.ProtocolTCP},
					{Name: "https", Port: portB, TargetPort: intstr.FromInt(targetB), Protocol: v1.ProtocolTCP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for the service to be reconciled with all pods registered")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, numPods)
		}, 90*time.Second, 10*time.Second).Should(Succeed(),
			"multi-port service should be reconciled in Azure and the Service Gateway")

		By(fmt.Sprintf("Verifying the LB has exactly one rule per port: [%d, %d]", portA, portB))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 60*time.Second, 5*time.Second).Should(Equal([]int32{portA, portB}),
			"the LB must have one rule per service port")

		utils.Logf("✓ Multi-port service provisioned %d LB rules with %d registered endpoints", 2, numPods)
	})

	It("should terminally reject a service whose ports share a protocol and backend port", func() {
		const (
			serviceName = "multiport-collision"
			numPods     = 3
			listenPort  = 8080
			portA       = int32(80)
			portB       = int32(443)
			sharedTgt   = 8080
		)
		labels := map[string]string{"app": serviceName}

		By("Creating backend pods")
		createReadyAgnhostPods(serviceName, labels, numPods, listenPort)

		By("Creating a LoadBalancer service whose two ports resolve to the same protocol + backend port")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					// Both ports map to TCP/8080 -> RulesUseSameBackendPortProtocolAndPool.
					{Name: "http", Port: portA, TargetPort: intstr.FromInt(sharedTgt), Protocol: v1.ProtocolTCP},
					{Name: "https", Port: portB, TargetPort: intstr.FromInt(sharedTgt), Protocol: v1.ProtocolTCP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Colliding service created with UID=%s", serviceUID)

		By("Verifying the service is terminally rejected and never provisions Azure resources")
		// The difftracker fails the build deterministically and parks the service, so no PIP,
		// LB, or Service Gateway registration is ever created. Assert this holds consistently
		// rather than just at a single instant.
		Consistently(func() bool {
			return verifyAzureResources(serviceUID) != nil
		}, 45*time.Second, 10*time.Second).Should(BeTrue(),
			"a service whose ports collide on protocol+backend port must be terminally rejected (no PIP/LB/SGW registration)")

		utils.Logf("✓ Colliding service was terminally rejected with no Azure resources provisioned")
	})

	It("should allow two ports sharing a backend port when their protocols differ (TCP + UDP)", func() {
		const (
			serviceName = "multiport-tcp-udp"
			numPods     = 3
			listenPort  = 8080
			port        = int32(80)
		)
		labels := map[string]string{"app": serviceName}

		By("Creating backend pods")
		createReadyAgnhostPods(serviceName, labels, numPods, listenPort)

		By("Creating a service with TCP and UDP on the same port and backend port")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					// Same backend port (8080) but different protocols is allowed: Azure scopes
					// the RulesUseSameBackendPortProtocolAndPool constraint per protocol.
					{Name: "tcp", Port: port, TargetPort: intstr.FromInt(listenPort), Protocol: v1.ProtocolTCP},
					{Name: "udp", Port: port, TargetPort: intstr.FromInt(listenPort), Protocol: v1.ProtocolUDP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("TCP+UDP service created with UID=%s", serviceUID)

		By("Waiting for the service to be reconciled")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, numPods)
		}, 90*time.Second, 10*time.Second).Should(Succeed(),
			"TCP+UDP service should be reconciled in Azure and the Service Gateway")

		By("Verifying the LB has one TCP and one UDP rule on the same backend port")
		rules, err := getLoadBalancerRules(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rules).To(HaveLen(2), "service should have one rule per protocol")
		protocols := map[string]int32{}
		for _, r := range rules {
			protocols[r.Protocol] = r.BackendPort
		}
		Expect(protocols).To(HaveKey("Tcp"), "a TCP rule must exist")
		Expect(protocols).To(HaveKey("Udp"), "a UDP rule must exist")
		Expect(protocols["Tcp"]).To(Equal(int32(listenPort)), "TCP rule backend port")
		Expect(protocols["Udp"]).To(Equal(int32(listenPort)), "UDP rule backend port")

		utils.Logf("✓ TCP+UDP service provisioned two rules on the same backend port")
	})
})
