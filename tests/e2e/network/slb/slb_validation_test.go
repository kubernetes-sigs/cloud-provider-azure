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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// The difftracker rejects two service shapes it cannot map to a PodIP backend, parking the
// service terminally (no Azure resources): a named targetPort (cannot be resolved to a
// concrete backend port) and a dual-stack service (a PodIP backend pool is single-family).
// These specs assert the service never provisions an LB.
var _ = Describe("SLB - Service Validation", Label(slbTestLabel), func() {
	basename := "slb-validation-test"

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

	// expectTerminallyRejected asserts the service never provisions Azure resources.
	expectTerminallyRejected := func(serviceUID, why string) {
		Consistently(func() bool {
			return verifyAzureResources(serviceUID) != nil
		}, 45*time.Second, 10*time.Second).Should(BeTrue(), why)
	}

	// expectServiceWarningEvent asserts a warning event with the given reason is recorded on the service.
	expectServiceWarningEvent := func(serviceName, reason string) {
		Eventually(func() bool {
			events, err := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				return false
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == serviceName && e.Reason == reason {
					return true
				}
			}
			return false
		}, 60*time.Second, 5*time.Second).Should(BeTrue(), "expected a "+reason+" warning event")
	}

	It("should terminally reject a service with a named targetPort", func() {
		const serviceName = "named-port-service"
		labels := map[string]string{"app": serviceName}

		By("Creating a LoadBalancer service whose targetPort is a name")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					// A named targetPort cannot be resolved to a concrete PodIP backend port.
					{Name: "http", Port: 80, TargetPort: intstr.FromString("http-port"), Protocol: v1.ProtocolTCP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Named-targetPort service created with UID=%s", serviceUID)

		By("Verifying the service is terminally rejected and never provisions Azure resources")
		expectTerminallyRejected(serviceUID,
			"a service with a named targetPort must be terminally rejected (no PIP/LB/SGW registration)")

		By("Verifying a warning event explains named targetPorts are unsupported")
		expectServiceWarningEvent(serviceName, "UnsupportedNamedTargetPort")

		utils.Logf("✓ Named-targetPort service was terminally rejected with no Azure resources")
	})

	It("should terminally reject a service with an SCTP port", func() {
		const serviceName = "sctp-service"
		labels := map[string]string{"app": serviceName}

		By("Creating a LoadBalancer service with an SCTP port")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					// The Azure (Service-SKU) load balancer only supports TCP/UDP; the
					// difftracker rejects SCTP at build time as an unsupported protocol.
					{Name: "sctp", Port: 90, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolSCTP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		if err != nil {
			// Some clusters disable SCTP at the API server; if so there is nothing to validate.
			if strings.Contains(err.Error(), "SCTP") {
				Skip("cluster does not allow SCTP services: " + err.Error())
			}
			Expect(err).NotTo(HaveOccurred())
		}
		serviceUID := string(created.UID)
		utils.Logf("SCTP service created with UID=%s", serviceUID)

		By("Verifying the service is terminally rejected and never provisions Azure resources")
		expectTerminallyRejected(serviceUID,
			"a service with an SCTP port must be terminally rejected (unsupported protocol)")

		utils.Logf("✓ SCTP service was terminally rejected with no Azure resources")
	})

	It("should terminally reject a dual-stack service", func() {
		const serviceName = "dualstack-service"
		labels := map[string]string{"app": serviceName}

		By("Creating a dual-stack LoadBalancer service")
		dualStack := v1.IPFamilyPolicyRequireDualStack
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:           v1.ServiceTypeLoadBalancer,
				Selector:       labels,
				IPFamilyPolicy: &dualStack,
				IPFamilies:     []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				Ports: []v1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		if err != nil {
			// A single-stack cluster rejects dual-stack at the K8s API; nothing to validate here.
			if strings.Contains(err.Error(), "IPv6") || strings.Contains(err.Error(), "dual") || strings.Contains(err.Error(), "ipFamilies") {
				Skip("cluster does not support dual-stack services: " + err.Error())
			}
			Expect(err).NotTo(HaveOccurred())
		}
		serviceUID := string(created.UID)
		utils.Logf("Dual-stack service created with UID=%s, ipFamilies=%v", serviceUID, created.Spec.IPFamilies)

		// If the cluster coerced the service to single-stack, the dual-stack rejection path is
		// not exercised; skip rather than assert the wrong thing.
		if len(created.Spec.IPFamilies) < 2 {
			Skip("cluster coerced the service to single-stack; dual-stack rejection not exercised")
		}

		By("Verifying the dual-stack service is terminally rejected and never provisions Azure resources")
		expectTerminallyRejected(serviceUID,
			"a dual-stack service must be terminally rejected (no PIP/LB/SGW registration)")

		By("Verifying a warning event explains dual-stack is unsupported")
		expectServiceWarningEvent(serviceName, "UnsupportedDualStack")

		utils.Logf("✓ Dual-stack service was terminally rejected with no Azure resources")
	})

	It("should reject an internal LoadBalancer service and surface a warning event", func() {
		const serviceName = "internal-service"
		labels := map[string]string{"app": serviceName}

		By("Creating a LoadBalancer service requesting an internal IP")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        serviceName,
				Namespace:   ns.Name,
				Annotations: map[string]string{"service.beta.kubernetes.io/azure-load-balancer-internal": "true"},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Internal service created with UID=%s", serviceUID)

		By("Verifying the service never provisions Azure resources and gets no ingress IP")
		expectTerminallyRejected(serviceUID,
			"an internal LoadBalancer must be rejected under ServiceGateway (no PIP/LB/SGW registration)")
		Consistently(func() int {
			svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if getErr != nil {
				return 0
			}
			return len(svc.Status.LoadBalancer.Ingress)
		}, 30*time.Second, 10*time.Second).Should(Equal(0), "rejected internal service must not receive an ingress IP")

		By("Verifying a warning event explains internal load balancers are unsupported")
		Eventually(func() bool {
			events, evErr := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
			if evErr != nil {
				return false
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == serviceName && e.Reason == "UnsupportedInternalLoadBalancer" {
					return true
				}
			}
			return false
		}, 60*time.Second, 5*time.Second).Should(BeTrue(), "expected an UnsupportedInternalLoadBalancer warning event")

		utils.Logf("✓ Internal service was rejected with a warning event and no Azure resources")
	})
})
