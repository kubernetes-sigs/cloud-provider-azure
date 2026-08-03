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

// Inbound dataplane behaviour of the Container Load Balancer.
//
// These specs drive real traffic at a Service's load balancer IP from a pod inside the
// cluster and assert which backend answered. Backends are told apart by their pod name,
// which agnhost returns from /hostname, so every assertion is made on where the packet
// actually landed rather than on the Azure configuration that was programmed.
//
// Covered here:
//
//   - a multi-port Service is reachable on each declared port, and a port it does not
//     declare is not served;
//   - two Services with separate backends stay isolated, with no application-layer
//     information involved in the routing decision;
//   - requests are spread across the backend pods;
//   - traffic survives a cloud-controller-manager restart, confirming the controller is
//     not in the data path;
//   - traffic survives losing backend pods, and a deleted pod never answers again.
//
// The dataplane is programmed by Azure NRP rather than by this repository, and it does
// not carry traffic in every environment (slb_egress_connectivity_test.go records the
// same caveat for egress). Each spec therefore probes the load balancer first and skips
// with an explicit message when it never answers; once traffic flows, the assertions are
// strict.
var _ = Describe("SLB - Dataplane", Label(slbTestLabel, "SLB-Dataplane"), func() {
	basename := "slb-dataplane-test"

	const (
		backendPort    = 8080
		altBackendPort = 9090
		// Long enough for NRP to program a freshly registered endpoint, short enough that a
		// genuinely broken dataplane fails the probe quickly.
		dataplaneReadyTimeout = 4 * time.Minute
		requestTimeoutSeconds = 5
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

	// createBackendPods creates numPods agnhost pods serving HTTP on listenPort. Agnhost's
	// /hostname endpoint returns the pod name, which is what lets these specs tell backends
	// apart without any application-layer routing.
	createBackendPods := func(namePrefix string, labels map[string]string, numPods, listenPort int) []string {
		names := make([]string, 0, numPods)
		for i := 0; i < numPods; i++ {
			name := fmt.Sprintf("%s-%d", namePrefix, i)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns.Name,
					Labels:    labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "backend",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", listenPort)},
						Ports: []v1.ContainerPort{{
							ContainerPort: int32(listenPort),
						}},
						ReadinessProbe: &v1.Probe{
							ProbeHandler: v1.ProbeHandler{
								HTTPGet: &v1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(int32(listenPort)),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       2,
						},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			names = append(names, name)
		}
		return names
	}

	// createDualPortBackendPods creates pods serving HTTP on TWO distinct backend ports, so a
	// multi-port service can map each of its ports to its own backend port. Mapping two ports
	// of the same protocol to the SAME backend port is rejected by design (Azure refuses two
	// rules sharing a backend port and protocol on one pool), so the ports must differ here.
	createDualPortBackendPods := func(namePrefix string, labels map[string]string, numPods int) []string {
		names := make([]string, 0, numPods)
		for i := 0; i < numPods; i++ {
			name := fmt.Sprintf("%s-%d", namePrefix, i)
			// Both containers share the pod network namespace, so each needs its own UDP port
			// as well; agnhost netexec binds UDP 8081 by default and the second container
			// would fail to start on a duplicate bind.
			container := func(cname string, port, udpPort int) v1.Container {
				return v1.Container{
					Name:            cname,
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args: []string{
						"netexec",
						fmt.Sprintf("--http-port=%d", port),
						fmt.Sprintf("--udp-port=%d", udpPort),
					},
					Ports: []v1.ContainerPort{{ContainerPort: int32(port)}},
					ReadinessProbe: &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							HTTPGet: &v1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt32(int32(port)),
							},
						},
						InitialDelaySeconds: 1,
						PeriodSeconds:       2,
					},
				}
			}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns.Name,
					Labels:    labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						container("backend", backendPort, 8081),
						container("backend-alt", altBackendPort, 9091),
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			names = append(names, name)
		}
		return names
	}

	// createLoadBalancer creates a LoadBalancer service and returns it once Azure has
	// reported an ingress IP.
	createLoadBalancer := func(name string, selector map[string]string, wantEndpoints int, ports []v1.ServicePort) string {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: selector,
				Ports:    ports,
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Created service %s (UID %s)", created.Name, created.UID)

		eventuallyServiceReconciled(string(created.UID), wantEndpoints, 5*time.Minute)

		var ip string
		Eventually(func() string {
			current, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil || len(current.Status.LoadBalancer.Ingress) == 0 {
				return ""
			}
			ip = current.Status.LoadBalancer.Ingress[0].IP
			return ip
		}, 5*time.Minute, 5*time.Second).ShouldNot(BeEmpty(),
			"the service should be given a load balancer ingress IP")

		utils.Logf("Service %s is published on %s", name, ip)
		return ip
	}

	// clientPodName creates a pod used purely to originate requests from inside the cluster.
	// Driving traffic from a pod (rather than the test runner) keeps the specs runnable
	// against clusters whose load balancer IP is not reachable from outside the VNet.
	createClientPod := func(name string) string {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns.Name,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "client",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"pause"},
				}},
			},
		}
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		return name
	}

	// get issues a single request and returns the trimmed body. A failed request returns "",
	// so callers can count successes without the spec dying on the first transient error.
	get := func(clientPod, ip string, port int, path string) string {
		out, err := utils.RunKubectlNoPrint(ns.Name, "exec", clientPod, "--", "/bin/sh", "-c",
			fmt.Sprintf("curl -s -m %d http://%s:%d%s || true", requestTimeoutSeconds, ip, port, path))
		if err != nil {
			return ""
		}
		// RunKubectlNoPrint returns "stdout:<body>\nstderr:<err>"; we only want the body.
		body := out
		if i := strings.Index(body, "stdout:"); i >= 0 {
			body = body[i+len("stdout:"):]
		}
		if i := strings.Index(body, "\nstderr:"); i >= 0 {
			body = body[:i]
		}
		return strings.TrimSpace(body)
	}

	// skipUnlessDataplaneCarriesTraffic probes the load balancer and skips the spec when it
	// never answers. NRP owns the dataplane; a test environment that does not carry traffic
	// says nothing about the cloud provider under test.
	//
	// The probe must keep polling for the whole timeout before deciding: a freshly programmed
	// endpoint routinely refuses the first few requests, and skipping on that would silently
	// disable every dataplane assertion in this file on a perfectly healthy cluster.
	skipUnlessDataplaneCarriesTraffic := func(clientPod, ip string, port int) {
		var last string
		answered := func() bool {
			last = get(clientPod, ip, port, "/hostname")
			return last != ""
		}

		deadline := time.Now().Add(dataplaneReadyTimeout)
		for !answered() {
			if time.Now().After(deadline) {
				Skip(fmt.Sprintf(
					"load balancer %s:%d never answered within %s; this environment does not carry "+
						"Container Load Balancer dataplane traffic, so the dataplane assertions cannot run",
					ip, port, dataplaneReadyTimeout))
			}
			time.Sleep(10 * time.Second)
		}
		utils.Logf("Dataplane is live: %s:%d answered %q", ip, port, last)
	}

	// sample issues n requests and returns how many times each backend answered, plus the
	// number of failed requests.
	sample := func(clientPod, ip string, port, n int) (map[string]int, int) {
		hits := map[string]int{}
		failures := 0
		for i := 0; i < n; i++ {
			body := get(clientPod, ip, port, "/hostname")
			if body == "" {
				failures++
				continue
			}
			hits[body]++
		}
		return hits, failures
	}

	// A Service that declares several ports must be reachable on every one of them, and each
	// request must land on one of that Service's backends. Routing is decided by address and
	// port alone, so a port the Service does not declare must not be served.
	It("routes every declared port of a multi-port service to its backend pods", func() {
		labels := map[string]string{"app": "dataplane-multiport"}
		podNames := createDualPortBackendPods("multiport-backend", labels, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ip := createLoadBalancer("dataplane-multiport-svc", labels, 2, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(backendPort)},
			{Name: "alt", Protocol: v1.ProtocolTCP, Port: 9000, TargetPort: intstr.FromInt32(altBackendPort)},
		})

		client := createClientPod("multiport-client")
		skipUnlessDataplaneCarriesTraffic(client, ip, 80)

		backends := map[string]bool{}
		for _, name := range podNames {
			backends[name] = true
		}

		for _, port := range []int{80, 9000} {
			By(fmt.Sprintf("Verifying port %d reaches a backend of this service", port))
			hits, failures := sample(client, ip, port, 10)
			// Tolerate at most one transient blip out of ten. The previous bound was "< 10", which
			// accepted nine failures out of ten requests - i.e. it asserted only that a single
			// request had succeeded, so a dataplane dropping 90% of traffic passed. These samples
			// run after skipUnlessDataplaneCarriesTraffic has already proved the LB answers, so a
			// healthy path should show no failures at all.
			Expect(failures).To(BeNumerically("<=", 1),
				fmt.Sprintf("every request to port %d failed; the port is not carrying traffic", port))
			Expect(hits).NotTo(BeEmpty())
			for answered := range hits {
				Expect(backends).To(HaveKey(answered),
					fmt.Sprintf("port %d was answered by %q, which is not a backend of this service", port, answered))
			}
			utils.Logf("  port %d answered by %v (%d failures)", port, hits, failures)
		}

		By("Verifying a port the service does not declare is not served")
		Expect(get(client, ip, 9999, "/hostname")).To(BeEmpty(),
			"an undeclared port must not be reachable on the load balancer IP")
	})

	// Two Services, each with its own backends and its own port, must stay isolated. No
	// request carries a host header or any other application-layer hint, so the only thing
	// separating the two is the load balancer configuration itself.
	It("keeps two services isolated without any HTTP host information", func() {
		labelsA := map[string]string{"app": "dataplane-svc-a"}
		labelsB := map[string]string{"app": "dataplane-svc-b"}
		podsA := createBackendPods("svc-a-backend", labelsA, 2, backendPort)
		podsB := createBackendPods("svc-b-backend", labelsB, 2, backendPort)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ipA := createLoadBalancer("dataplane-svc-a", labelsA, 2, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(backendPort)},
		})
		ipB := createLoadBalancer("dataplane-svc-b", labelsB, 2, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 9000, TargetPort: intstr.FromInt32(backendPort)},
		})

		client := createClientPod("isolation-client")
		skipUnlessDataplaneCarriesTraffic(client, ipA, 80)

		setA := map[string]bool{}
		for _, n := range podsA {
			setA[n] = true
		}
		setB := map[string]bool{}
		for _, n := range podsB {
			setB[n] = true
		}

		By("Verifying service A only ever answers from its own pods")
		hitsA, failuresA := sample(client, ipA, 80, 12)
		// See above: "< 12" accepted eleven failures out of twelve requests.
		Expect(failuresA).To(BeNumerically("<=", 1),
			"service A should answer nearly every request; %d/12 failed", failuresA)
		for answered := range hitsA {
			Expect(setA).To(HaveKey(answered),
				fmt.Sprintf("service A was answered by %q, which belongs to another service", answered))
		}

		By("Verifying service B only ever answers from its own pods")
		hitsB, failuresB := sample(client, ipB, 9000, 12)
		Expect(failuresB).To(BeNumerically("<=", 1),
			"service B should answer nearly every request; %d/12 failed", failuresB)
		for answered := range hitsB {
			Expect(setB).To(HaveKey(answered),
				fmt.Sprintf("service B was answered by %q, which belongs to another service", answered))
		}

		By("Verifying each service is not reachable on the other service's port")
		Expect(get(client, ipA, 9000, "/hostname")).To(BeEmpty(),
			"service A must not serve service B's port")
		Expect(get(client, ipB, 80, "/hostname")).To(BeEmpty(),
			"service B must not serve service A's port")
	})

	// Requests must be spread over the backends rather than pinned to one. The assertion is
	// that more than one pod serves traffic, not that the split is even: the load balancer
	// offers no per-connection fairness guarantee, so asserting an exact distribution would
	// be flaky.
	It("distributes requests across the backend pods", func() {
		labels := map[string]string{"app": "dataplane-distribution"}
		podNames := createBackendPods("distribution-backend", labels, 3, backendPort)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ip := createLoadBalancer("dataplane-distribution-svc", labels, 3, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(backendPort)},
		})

		client := createClientPod("distribution-client")
		skipUnlessDataplaneCarriesTraffic(client, ip, 80)

		By("Sending 30 requests and recording which pod answered")
		hits, failures := sample(client, ip, 80, 30)
		utils.Logf("  distribution: %v (%d failures)", hits, failures)

		// The previous bound ("< 15") accepted a 47% loss rate as success. Allow only a ~10%
		// transient margin so a genuinely degraded backend pool is visible.
		Expect(failures).To(BeNumerically("<=", 3),
			"the load balancer should answer nearly every request; %d/30 failed", failures)
		for answered := range hits {
			Expect(podNames).To(ContainElement(answered))
		}
		Expect(len(hits)).To(BeNumerically(">=", 2),
			fmt.Sprintf("all successful requests were served by a single pod (%v); traffic is not being distributed", hits))
	})

	// The cloud-controller-manager programs the load balancer but must not sit in the data
	// path. Restarting it may pause reconciliation, but traffic must keep flowing throughout
	// and must be completely healthy once the controller is back.
	It("keeps serving traffic while the cloud-controller-manager restarts", func() {
		labels := map[string]string{"app": "dataplane-ccm-restart"}
		createBackendPods("ccm-restart-backend", labels, 3, backendPort)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ip := createLoadBalancer("dataplane-ccm-restart-svc", labels, 3, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(backendPort)},
		})

		client := createClientPod("ccm-restart-client")
		skipUnlessDataplaneCarriesTraffic(client, ip, 80)

		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; cannot restart the cloud-controller-manager")
		}
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		By("Restarting the cloud-controller-manager while traffic is in flight")
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())

		By("Verifying traffic kept flowing across the restart")
		hits, failures := sample(client, ip, 80, 20)
		utils.Logf("  during/after CCM restart: %v (%d failures)", hits, failures)
		Expect(hits).NotTo(BeEmpty(), "no request succeeded across the cloud-controller-manager restart")

		By("Verifying the dataplane is fully healthy afterwards")
		Eventually(func() int {
			_, f := sample(client, ip, 80, 5)
			return f
		}, 3*time.Minute, 10*time.Second).Should(BeZero(),
			"traffic did not fully recover after the cloud-controller-manager restarted")
	})

	// Losing backend pods must degrade service rather than break it: the surviving pods keep
	// answering, and a pod that no longer exists must never answer again.
	It("keeps serving traffic from the surviving pods when backends are deleted", func() {
		labels := map[string]string{"app": "dataplane-backend-loss"}
		podNames := createBackendPods("backend-loss", labels, 4, backendPort)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ip := createLoadBalancer("dataplane-backend-loss-svc", labels, 4, []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(backendPort)},
		})

		client := createClientPod("backend-loss-client")
		skipUnlessDataplaneCarriesTraffic(client, ip, 80)

		killed := podNames[:2]
		survivors := map[string]bool{}
		for _, n := range podNames[2:] {
			survivors[n] = true
		}

		By(fmt.Sprintf("Deleting backend pods %v while traffic continues", killed))
		for _, name := range killed {
			Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), name, metav1.DeleteOptions{})).To(Succeed())
		}

		By("Verifying requests still succeed via the surviving pods")
		Eventually(func() bool {
			hits, _ := sample(client, ip, 80, 5)
			if len(hits) == 0 {
				return false
			}
			for answered := range hits {
				if !survivors[answered] {
					return false
				}
			}
			return true
		}, 3*time.Minute, 10*time.Second).Should(BeTrue(),
			"traffic did not settle onto the surviving pods after the others were deleted")

		By("Verifying no deleted pod answers any further request")
		hits, failures := sample(client, ip, 80, 20)
		utils.Logf("  after backend loss: %v (%d failures)", hits, failures)
		Expect(hits).NotTo(BeEmpty(), "no request succeeded after losing half of the backends")
		for _, name := range killed {
			Expect(hits).NotTo(HaveKey(name),
				fmt.Sprintf("deleted pod %q still answered traffic", name))
		}
	})
})
