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
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// Edge cases around the inbound service shape: a service with many distinct ports, and several
// services that select the same pods. Both assert the cloud-provider contract (LB rules and pod
// registrations) and are independent of the environment dataplane.
var _ = Describe("SLB - Service Config Edge Cases", Label(slbTestLabel), func() {
	basename := "slb-service-config-test"

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

	makeNetexecPod := func(name string, labels map[string]string, targetPort int) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
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

	expectServiceEvent := func(serviceName, reason string) {
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
		}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "expected a "+reason+" event")
	}

	It("should create one LB rule per port for a service with many distinct ports", func() {
		const (
			numPods    = 2
			numPorts   = 6
			basePort   = int32(8000)
			baseTarget = 9000
			waitTime   = 90 * time.Second
		)
		serviceName := "many-ports-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			// Pods only need to be Ready to register as endpoints; LB rules come from the
			// Service spec, so the pods do not have to listen on every target port.
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, baseTarget), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating a service with %d distinct ports", numPorts))
		ports := make([]v1.ServicePort, 0, numPorts)
		wantPorts := make([]int32, 0, numPorts)
		for i := 0; i < numPorts; i++ {
			fePort := basePort + int32(i)
			ports = append(ports, v1.ServicePort{
				Name:       fmt.Sprintf("p%d", i),
				Port:       fePort,
				TargetPort: intstr.FromInt(baseTarget + i),
				Protocol:   v1.ProtocolTCP,
			})
			wantPorts = append(wantPorts, fePort)
		}
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    ports,
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)

		By("Waiting for the service to provision and register its pods")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By(fmt.Sprintf("Verifying the LB has exactly %d rules, one per service port", numPorts))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 60*time.Second, 5*time.Second).Should(Equal(wantPorts),
			"the LB must have one rule per service port")

		utils.Logf("\n✓ Many-port service produced %d LB rules", numPorts)
	})

	It("should apply Public IP tags and the DNS label and keep the IP when they change", func() {
		const serviceName = "pip-settings-service"
		labels := map[string]string{"app": serviceName}
		dnsLabel := "sgwe2e-" + rand.String(8)

		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(serviceName+"-pod", labels, 8080), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a service with Public IP tags, a reserved tag key and a DNS label")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-pip-tags":       "sgw-e2e=first,k8s-azure-service=spoof",
					"service.beta.kubernetes.io/azure-dns-label-name": dnsLabel,
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    []v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		eventuallyServiceReconciled(serviceUID, 1, 3*time.Minute)

		expectPublicIP := func(tagValue, label string) string {
			var ip string
			Eventually(func() error {
				pip, err := getAzurePublicIP(serviceUID + "-pip")
				if err != nil {
					return err
				}
				switch {
				case pip.Tags["sgw-e2e"] != tagValue:
					return fmt.Errorf("tag sgw-e2e=%q, want %q", pip.Tags["sgw-e2e"], tagValue)
				case pip.Tags["k8s-azure-service"] != ns.Name+"/"+serviceName:
					return fmt.Errorf("ownership tag k8s-azure-service=%q", pip.Tags["k8s-azure-service"])
				case pip.Tags["k8s-azure-cluster-name"] == "":
					return fmt.Errorf("ownership tag k8s-azure-cluster-name is missing")
				case pip.DNSSettings == nil || pip.DNSSettings.DomainNameLabel != label:
					return fmt.Errorf("DNS label %+v, want %q", pip.DNSSettings, label)
				}
				ip = pip.IPAddress
				return nil
			}, 3*time.Minute, 10*time.Second).Should(Succeed())
			return ip
		}

		By("Verifying the Public IP carries the tags, the ownership tags and the DNS label")
		ip := expectPublicIP("first", dnsLabel)

		By("Verifying the reserved tag key is reported")
		expectServiceEvent(serviceName, "IgnoredPIPTagKeys")

		By("Changing the tag and the DNS label")
		updatedLabel := dnsLabel + "b"
		Eventually(func() error {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			svc.Annotations["service.beta.kubernetes.io/azure-pip-tags"] = "sgw-e2e=second"
			svc.Annotations["service.beta.kubernetes.io/azure-dns-label-name"] = updatedLabel
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("Verifying the Public IP is updated in place")
		Expect(expectPublicIP("second", updatedLabel)).To(Equal(ip), "changing tags or the DNS label must not change the IP")

		utils.Logf("✓ Public IP tags and DNS label were applied and updated without changing the IP")
	})

	It("should allocate the Public IP from a Public IP prefix and keep it when the prefix annotation changes", func() {
		const serviceName = "pip-prefix-service"
		labels := map[string]string{"app": serviceName}
		prefixName := "sgwe2e-prefix-" + rand.String(6)

		By("Creating a StandardV2 Public IP prefix")
		location, err := runAz("group", "show", "--name", resourceGroupName, "--query", "location", "--output", "tsv")
		Expect(err).NotTo(HaveOccurred())
		prefixID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPPrefixes/%s", subscriptionID, resourceGroupName, prefixName)
		body := fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"prefixLength":31,"publicIPAddressVersion":"IPv4"}}`, strings.TrimSpace(string(location)))
		_, err = runAz("rest", "--method", "put", "--url", "https://management.azure.com"+prefixID+"?api-version=2025-05-01", "--body", body)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Eventually(func() error {
				_, err := runAz("rest", "--method", "delete", "--url", "https://management.azure.com"+prefixID+"?api-version=2025-05-01")
				return err
			}, 5*time.Minute, 15*time.Second).Should(Succeed(), "the test prefix must be deleted")
		})
		var prefixRange string
		Eventually(func() error {
			out, err := runAz("rest", "--method", "get", "--url", "https://management.azure.com"+prefixID+"?api-version=2025-05-01", "--query", "properties.ipPrefix", "--output", "tsv")
			prefixRange = strings.TrimSpace(string(out))
			if err == nil && prefixRange == "" {
				err = fmt.Errorf("prefix %s has no address range yet", prefixName)
			}
			return err
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
		allocated, err := netip.ParsePrefix(prefixRange)
		Expect(err).NotTo(HaveOccurred())

		_, err = cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(serviceName+"-pod", labels, 8080), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a service that uses the prefix")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        serviceName,
				Namespace:   ns.Name,
				Annotations: map[string]string{"service.beta.kubernetes.io/azure-pip-prefix-id": prefixID},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    []v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		eventuallyServiceReconciled(serviceUID, 1, 3*time.Minute)

		By("Verifying the Public IP address comes from the prefix")
		pip, err := getAzurePublicIP(serviceUID + "-pip")
		Expect(err).NotTo(HaveOccurred())
		ip, err := netip.ParseAddr(pip.IPAddress)
		Expect(err).NotTo(HaveOccurred())
		Expect(allocated.Contains(ip)).To(BeTrue(), "Public IP %s must come from prefix %s", ip, allocated)

		By("Pointing the annotation at another prefix")
		otherPrefixID := prefixID + "-other"
		Eventually(func() error {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			svc.Annotations["service.beta.kubernetes.io/azure-pip-prefix-id"] = otherPrefixID
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("Verifying the change is reported and the Public IP keeps its address")
		expectServiceEvent(serviceName, "PublicIPPrefixChangeNotSupported")
		pip, err = getAzurePublicIP(serviceUID + "-pip")
		Expect(err).NotTo(HaveOccurred())
		Expect(pip.IPAddress).To(Equal(ip.String()), "a prefix change must not change the IP")

		utils.Logf("✓ Public IP %s was allocated from prefix %s and kept when the annotation changed", ip, allocated)
	})

	createService := func(name string, annotations map[string]string) string {
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Annotations: annotations},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: map[string]string{"app": name},
				Ports:    []v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return string(created.UID)
	}

	updateAnnotations := func(name string, mutate func(map[string]string)) {
		Eventually(func() error {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if svc.Annotations == nil {
				svc.Annotations = map[string]string{}
			}
			mutate(svc.Annotations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	}

	// expectServiceOnPublicIP waits until the Service's load balancer uses the Public IP and the Service
	// status reports its address, and returns that address.
	expectServiceOnPublicIP := func(name, serviceUID, publicIPName string) string {
		var address string
		Eventually(func() error {
			if err := serviceUsesPublicIPErr(serviceUID, publicIPID(publicIPName)); err != nil {
				return err
			}
			pip, err := getAzurePublicIP(publicIPName)
			if err != nil {
				return err
			}
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if len(svc.Status.LoadBalancer.Ingress) != 1 || svc.Status.LoadBalancer.Ingress[0].IP != pip.IPAddress {
				return fmt.Errorf("service %s ingress %+v, want %s", name, svc.Status.LoadBalancer.Ingress, pip.IPAddress)
			}
			address = pip.IPAddress
			return nil
		}, 4*time.Minute, 10*time.Second).Should(Succeed())
		return address
	}

	// probeTraffic waits for the pod to be registered and returns an error when traffic to the address never
	// reaches it. The dataplane depends on the environment, so specs finish their strict checks first and
	// then skip on this error, as the other reachability specs do.
	probeTraffic := func(serviceUID, address, podName string, port int) error {
		Eventually(func() (int, error) { return countRegisteredEndpoints(serviceUID) }, 4*time.Minute, 10*time.Second).Should(Equal(1))
		return verifyAllPodsReachable(address, port, []string{podName}, 4*time.Minute)
	}
	skipIfNoTraffic := func(err error) {
		if err != nil {
			Skip("the Azure checks passed but the dataplane did not carry traffic from the test runner: " + err.Error())
		}
	}

	expectUserPublicIPUntouched := func(publicIPName string) {
		Eventually(func() error {
			pip, err := getAzurePublicIP(publicIPName)
			if err != nil {
				return err
			}
			if owner, ok := pip.Tags["k8s-azure-service"]; ok {
				return fmt.Errorf("the user's Public IP %s was tagged k8s-azure-service=%q", publicIPName, owner)
			}
			if pip.IPConfiguration != nil {
				return fmt.Errorf("the user's Public IP %s is still used by %s", publicIPName, pip.IPConfiguration.ID)
			}
			return nil
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
	}

	It("should use Public IPs chosen by name or address, keeping the user's and deleting the one it created", func() {
		userPIPName := "sgwe2e-user-" + rand.String(6)
		createdPIPName := "sgwe2e-created-" + rand.String(6)
		_, userIP := createUserPublicIP(userPIPName, "StandardV2")
		DeferCleanup(func() {
			_, _ = runAz("network", "public-ip", "delete", "--resource-group", resourceGroupName, "--name", createdPIPName)
		})

		for _, name := range []string{"byo-user", "byo-created"} {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(name+"-pod", map[string]string{"app": name}, 8080), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a service on the user's Public IP and one naming a Public IP that does not exist")
		userUID := createService("byo-user", map[string]string{"service.beta.kubernetes.io/azure-pip-name": userPIPName})
		createdUID := createService("byo-created", map[string]string{"service.beta.kubernetes.io/azure-pip-name": createdPIPName})

		By("Verifying each load balancer uses the chosen Public IP and the status reports its address")
		Expect(expectServiceOnPublicIP("byo-user", userUID, userPIPName)).To(Equal(userIP))
		expectServiceOnPublicIP("byo-created", createdUID, createdPIPName)
		Expect(azurePublicIPAbsentErr(userUID)).To(Succeed(), "no Public IP is created for a Service that names the user's")
		Expect(azurePublicIPAbsentErr(createdUID)).To(Succeed())

		By("Verifying only the created Public IP carries the ownership tags")
		created, err := getAzurePublicIP(createdPIPName)
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Tags).To(HaveKeyWithValue("k8s-azure-service", ns.Name+"/byo-created"))
		user, err := getAzurePublicIP(userPIPName)
		Expect(err).NotTo(HaveOccurred())
		Expect(user.Tags).NotTo(HaveKey("k8s-azure-service"))

		By("Probing traffic to the user's Public IP")
		trafficErr := probeTraffic(userUID, userIP, "byo-user-pod", 80)

		By("Choosing the created Public IP by its address instead, together with a new port")
		createdIP := created.IPAddress
		Eventually(func() error {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), "byo-created", metav1.GetOptions{})
			if err != nil {
				return err
			}
			delete(svc.Annotations, "service.beta.kubernetes.io/azure-pip-name")
			svc.Annotations["service.beta.kubernetes.io/azure-load-balancer-ipv4"] = createdIP
			svc.Spec.Ports[0].Port = 81
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("Verifying the update is applied on the same Public IP")
		Eventually(func() error {
			if err := serviceUsesPublicIPErr(createdUID, publicIPID(createdPIPName)); err != nil {
				return err
			}
			output, err := runAz("network", "lb", "show", "--resource-group", resourceGroupName, "--name", createdUID, "--output", "json")
			if err != nil {
				return err
			}
			var lb AzureLoadBalancer
			if err := json.Unmarshal(output, &lb); err != nil {
				return err
			}
			if len(lb.LoadBalancingRules) != 1 || lb.LoadBalancingRules[0].Name != "rule-tcp-81" {
				return fmt.Errorf("load balancer rules %+v, want only rule-tcp-81", lb.LoadBalancingRules)
			}
			return nil
		}, 4*time.Minute, 10*time.Second).Should(Succeed())
		controllerWarnings := map[string]bool{
			"PublicIPNotFound": true, "PublicIPInUse": true, "PublicIPCleanupFailed": true, "ServiceGatewayConfigurationRejected": true,
			"InvalidLoadBalancerIP": true, "ConflictingPublicIPSettings": true, "ServiceGatewayIgnoredAnnotations": true,
			"PublicIPChangeNotSupported": true, "PublicIPPrefixChangeNotSupported": true,
		}
		events, err := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == "Service" && string(e.InvolvedObject.UID) == createdUID && controllerWarnings[e.Reason] {
				Fail(fmt.Sprintf("unexpected warning %s on byo-created: %s", e.Reason, e.Message))
			}
		}

		By("Deleting both services")
		for _, name := range []string{"byo-user", "byo-created"} {
			Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), name, metav1.DeleteOptions{})).To(Succeed())
		}
		eventuallyServiceDeleted(userUID, 3*time.Minute)
		eventuallyServiceDeleted(createdUID, 3*time.Minute)

		By("Verifying the user's Public IP is kept and free, and the created one is deleted")
		expectUserPublicIPUntouched(userPIPName)
		Eventually(func() error { return azurePublicIPNamedAbsentErr(createdPIPName) }, 3*time.Minute, 10*time.Second).Should(Succeed())

		utils.Logf("✓ The user's Public IP was used and kept; the Public IP created by name was deleted with its Service")
		skipIfNoTraffic(trafficErr)
	})

	It("should keep a service's Public IP when its choice changes", func() {
		const serviceName = "byo-keep"
		userPIPName := "sgwe2e-user-" + rand.String(6)
		otherName := "sgwe2e-other-" + rand.String(6)
		_, userIP := createUserPublicIP(userPIPName, "StandardV2")
		DeferCleanup(func() {
			_, _ = runAz("network", "public-ip", "delete", "--resource-group", resourceGroupName, "--name", otherName)
		})

		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(serviceName+"-pod", map[string]string{"app": serviceName}, 8080), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a service that chooses the user's Public IP by address")
		serviceUID := createService(serviceName, map[string]string{"service.beta.kubernetes.io/azure-load-balancer-ipv4": userIP})
		Expect(expectServiceOnPublicIP(serviceName, serviceUID, userPIPName)).To(Equal(userIP))

		By("Probing traffic to the user's Public IP as the baseline")
		baselineErr := probeTraffic(serviceUID, userIP, serviceName+"-pod", 80)

		expectKept := func() {
			expectServiceEvent(serviceName, "PublicIPChangeNotSupported")
			Consistently(func() error {
				if err := serviceUsesPublicIPErr(serviceUID, publicIPID(userPIPName)); err != nil {
					return err
				}
				return azurePublicIPNamedAbsentErr(otherName)
			}, 30*time.Second, defaultPollInterval).Should(Succeed(), "the load balancer keeps its Public IP and no other Public IP is created")
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.Status.LoadBalancer.Ingress).To(HaveLen(1))
			Expect(svc.Status.LoadBalancer.Ingress[0].IP).To(Equal(userIP))
			if baselineErr == nil {
				Expect(probeTraffic(serviceUID, userIP, serviceName+"-pod", 80)).To(Succeed(), "the kept address keeps serving")
			}
		}

		By("Choosing a Public IP by a name that does not exist")
		updateAnnotations(serviceName, func(a map[string]string) {
			delete(a, "service.beta.kubernetes.io/azure-load-balancer-ipv4")
			a["service.beta.kubernetes.io/azure-pip-name"] = otherName
		})
		expectKept()

		By("Deleting the service")
		Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})).To(Succeed())
		eventuallyServiceDeleted(serviceUID, 3*time.Minute)

		By("Verifying the user's Public IP is kept and free")
		expectUserPublicIPUntouched(userPIPName)

		utils.Logf("✓ The service kept Public IP %s when its choice changed", userIP)
		skipIfNoTraffic(baselineErr)
	})

	It("should reject a user's Public IP it cannot use without changing it", func() {
		standardPIPName := "sgwe2e-standard-" + rand.String(6)
		userPIPName := "sgwe2e-user-" + rand.String(6)
		createUserPublicIP(standardPIPName, "Standard")
		createUserPublicIP(userPIPName, "StandardV2")

		By("Creating a service on a Standard Public IP and one that would tag the user's Public IP")
		standardUID := createService("byo-standard", map[string]string{"service.beta.kubernetes.io/azure-pip-name": standardPIPName})
		taggedUID := createService("byo-tagged", map[string]string{
			"service.beta.kubernetes.io/azure-pip-name": userPIPName,
			"service.beta.kubernetes.io/azure-pip-tags": "sgw-e2e=first",
		})

		By("Verifying both services are rejected")
		expectServiceEvent("byo-standard", "ServiceGatewayConfigurationRejected")
		expectServiceEvent("byo-tagged", "ServiceGatewayConfigurationRejected")

		By("Verifying no load balancer was created and neither Public IP was changed")
		Consistently(func() error {
			for _, uid := range []string{standardUID, taggedUID} {
				if err := serviceDeletedErr(uid); err != nil {
					return err
				}
			}
			for _, name := range []string{standardPIPName, userPIPName} {
				pip, err := getAzurePublicIP(name)
				if err != nil {
					return err
				}
				if len(pip.Tags) != 0 || pip.IPConfiguration != nil {
					return fmt.Errorf("public IP %s was changed: tags %v, used by %+v", name, pip.Tags, pip.IPConfiguration)
				}
			}
			return nil
		}, 45*time.Second, defaultPollInterval).Should(Succeed())

		utils.Logf("✓ Unusable user Public IPs were rejected and left unchanged")
	})

	It("should let two services that select the same pods each register those pods", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		labels := map[string]string{"app": "shared-backend"}

		By(fmt.Sprintf("Creating %d shared pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("shared-pod-%d", i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		newSharedService := func(name string) string {
			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
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
			created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			return string(created.UID)
		}

		By("Creating two LoadBalancer services that select the same pods")
		uidA := newSharedService("shared-svc-a")
		uidB := newSharedService("shared-svc-b")

		By("Verifying each service independently provisions and registers all shared pods")
		eventuallyServiceReconciled(uidA, numPods, waitTime)
		eventuallyServiceReconciled(uidB, numPods, waitTime)

		utils.Logf("\n✓ Shared-pod services each registered %d pods (UIDs %s, %s)", numPods, uidA, uidB)
	})
})
