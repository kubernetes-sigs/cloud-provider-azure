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

package servicegateway

import (
	"context"
	"fmt"
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

// The difftracker rejects service shapes it cannot map to a PodIP backend, parking the service
// terminally (no Azure resources): a named targetPort (cannot be resolved to a concrete backend
// port) and a protocol other than TCP and UDP. These specs assert the service never provisions an LB.
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

	// expectTerminallyRejected asserts the service is genuinely refused rather than merely slow.
	//
	// Waiting for verifyAzureResources to keep returning *an* error is not enough on its own: it
	// short-circuits on the first missing resource, so it never reaches its Service Gateway check,
	// and a perfectly valid service that is simply still provisioning (60-180s on live Azure)
	// satisfies it too. Assert each resource is absent in its own right, and require the warning
	// event as the positive signal that the CCM decided to reject rather than not having got to it.
	expectTerminallyRejected := func(serviceName, serviceUID, reason, why string) {
		By("Verifying a warning event records the rejection reason")
		expectServiceWarningEvent(serviceName, reason)

		By("Verifying no Azure or Service Gateway resources were provisioned")
		Consistently(func() error {
			// No Service Gateway registration.
			if err := serviceDeletedErr(serviceUID); err != nil {
				return err
			}
			// No Public IP.
			if err := azurePublicIPAbsentErr(serviceUID); err != nil {
				return err
			}
			// No Service status ingress.
			svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("get service %s: %w", serviceName, getErr)
			}
			if len(svc.Status.LoadBalancer.Ingress) != 0 {
				return fmt.Errorf("service %s was assigned an ingress IP despite being rejected", serviceName)
			}
			return nil
		}, 45*time.Second, defaultPollInterval).Should(Succeed(), why)
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
		expectTerminallyRejected(serviceName, serviceUID, "UnsupportedNamedTargetPort",
			"a service with a named targetPort must be terminally rejected (no PIP/LB/SGW registration)")

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
		expectTerminallyRejected(serviceName, serviceUID, "UnsupportedProtocol",
			"a service with an SCTP port must be terminally rejected (unsupported protocol)")

		utils.Logf("✓ SCTP service was terminally rejected with no Azure resources")
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
		expectTerminallyRejected(serviceName, serviceUID, "UnsupportedInternalLoadBalancer",
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

	It("should reject services that restrict access, select their Public IP inconsistently or carry settings without effect", func() {
		cases := []struct {
			name   string
			reason string
			mutate func(*v1.Service)
		}{
			{"source-ranges", "UnsupportedAccessRestriction", func(s *v1.Service) {
				s.Spec.LoadBalancerSourceRanges = []string{"203.0.113.0/24"}
			}},
			{"allowed-service-tags", "UnsupportedAccessRestriction", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-allowed-service-tags": "AzureCloud"}
			}},
			{"no-lb-rule", "UnsupportedAccessRestriction", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/port_80_no_lb_rule": "true"}
			}},
			{"private-link", "UnsupportedPrivateLinkService", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-pls-create": "true"}
			}},
			{"invalid-address", "InvalidLoadBalancerIP", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-load-balancer-ipv4": "203.0.113"}
			}},
			{"name-and-address", "ConflictingPublicIPSettings", func(s *v1.Service) {
				s.Annotations = map[string]string{
					"service.beta.kubernetes.io/azure-pip-name":           "customer-pip",
					"service.beta.kubernetes.io/azure-load-balancer-ipv4": "203.0.113.10",
				}
			}},
			{"deny-all", "UnsupportedAccessRestriction", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges": "true"}
			}},
			{"floating-ip", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip": "true"}
			}},
			{"no-probe-rule", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/port_80_no_probe_rule": "true"}
			}},
			{"additional-public-ips", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-additional-public-ips": "203.0.113.20"}
			}},
			{"lb-mode", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-load-balancer-mode": "auto"}
			}},
			{"health-probe", "UnsupportedHealthProbe", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path": "/healthz"}
			}},
			{"reserved-pip-tag", "InvalidPIPTags", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-pip-tags": "team=a,k8s-azure-service=spoof"}
			}},
			{"malformed-pip-tags", "InvalidPIPTags", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-pip-tags": "team:a"}
			}},
			{"malformed-ip-tags", "InvalidIPTags", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-pip-ip-tags": "FirstPartyUsage=/Unprivileged=x"}
			}},
			{"conflicting-load-balancer-ip", "ConflictingPublicIPSettings", func(s *v1.Service) {
				s.Spec.LoadBalancerIP = "203.0.113.10"
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-load-balancer-ipv4": "203.0.113.20"}
			}},
			{"resource-group-without-public-ip", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-load-balancer-resource-group": "rg"}
			}},
			{"ipv6-pip-name-on-ipv4-service", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/azure-pip-name-ipv6": "x"}
			}},
			{"no-lb-rule-for-non-service-port", "UnsupportedAnnotations", func(s *v1.Service) {
				s.Annotations = map[string]string{"service.beta.kubernetes.io/port_8080_no_lb_rule": "true"}
			}},
		}

		uids := map[string]string{}
		for _, tc := range cases {
			By("Creating the " + tc.name + " service")
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: ns.Name},
				Spec: v1.ServiceSpec{
					Type:     v1.ServiceTypeLoadBalancer,
					Selector: map[string]string{"app": tc.name},
					Ports:    []v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP}},
				},
			}
			tc.mutate(service)
			created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			uids[tc.name] = string(created.UID)
		}

		for _, tc := range cases {
			By("Verifying the " + tc.name + " service is rejected with " + tc.reason)
			expectServiceWarningEvent(tc.name, tc.reason)
		}

		By("Verifying none of the services provisioned Azure or Service Gateway resources")
		Consistently(func() error {
			for _, tc := range cases {
				if err := serviceDeletedErr(uids[tc.name]); err != nil {
					return err
				}
				if err := azurePublicIPAbsentErr(uids[tc.name]); err != nil {
					return err
				}
				svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), tc.name, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("get service %s: %w", tc.name, err)
				}
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					return fmt.Errorf("service %s was assigned an ingress IP despite being rejected", tc.name)
				}
			}
			return nil
		}, 45*time.Second, defaultPollInterval).Should(Succeed(),
			"a setting ServiceGateway cannot honour must be rejected (no PIP/LB/SGW registration)")

		utils.Logf("✓ Services with unsupported access or inconsistent Public IP settings were rejected")
	})

	It("should provision a service whose source ranges allow every address", func() {
		const serviceName = "allow-all-service"
		labels := map[string]string{"app": serviceName}

		By("Creating a service whose source ranges allow every address")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        serviceName,
				Namespace:   ns.Name,
				Annotations: map[string]string{"service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges": "false"},
			},
			Spec: v1.ServiceSpec{
				Type:                     v1.ServiceTypeLoadBalancer,
				Selector:                 labels,
				LoadBalancerSourceRanges: []string{"0.0.0.0/0"},
				Ports:                    []v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the service is provisioned")
		eventuallyServiceReconciled(string(created.UID), -1, 3*time.Minute)

		utils.Logf("✓ Allow-all service was provisioned")
	})
})
