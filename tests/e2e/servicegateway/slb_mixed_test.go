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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Mixed Workload", Label(slbTestLabel), func() {
	basename := "slb-mixed-test"

	var (
		cs               clientset.Interface
		ns               *v1.Namespace
		sourcePolicyUIDs []string
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
		sourcePolicyUIDs = nil
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			if len(sourcePolicyUIDs) > 0 {
				Eventually(func() error {
					groups, err := readNICNSGs()
					if err != nil {
						return err
					}
					for _, group := range groups {
						for _, uid := range sourcePolicyUIDs {
							if len(nicNSGServiceRules(group, uid)) > 0 {
								return fmt.Errorf("NSG %s retains deleted Service %s", group.ID, uid)
							}
						}
					}
					return nil
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
			}

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

	// Run before the legacy Node-deletion spec; these cases require three Nodes.
	DescribeTable("keeps NIC source policies and outbound SNAT correct while shared backend Pods scale",
		Label("SLB-NIC-NSG", "SLB-NIC-SCALE"), SpecPriority(1), func(family v1.IPFamily) {
			ctx := context.Background()
			nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			var selected []string
			for _, node := range nodes.Items {
				ready := false
				for _, condition := range node.Status.Conditions {
					ready = ready || condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue
				}
				for _, address := range node.Status.Addresses {
					ip, parseErr := netip.ParseAddr(address.Address)
					if ready && !node.Spec.Unschedulable && address.Type == v1.NodeInternalIP && parseErr == nil &&
						ip.Is6() == (family == v1.IPv6Protocol) {
						selected = append(selected, node.Name)
						break
					}
				}
			}
			if family == v1.IPv6Protocol && len(selected) == 0 {
				Skip("requires IPv6 Nodes")
			}
			Expect(len(selected)).To(BeNumerically(">=", 3), "requires two backend Nodes and an independent client Node")
			sort.Strings(selected)
			selected = selected[:3]
			app := map[string]string{"app": "nsg-scale"}
			egressName := "nsg-scale-" + string(ns.UID)
			policy := v1.IPFamilyPolicySingleStack
			service, err := cs.CoreV1().Services(ns.Name).Create(ctx, &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "nsg-scale", Namespace: ns.Name},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, Selector: app, IPFamilies: []v1.IPFamily{family}, IPFamilyPolicy: &policy,
					Ports: []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			uid := string(service.UID)
			sourcePolicyUIDs = append(sourcePolicyUIDs, uid)
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "client", Namespace: ns.Name},
				Spec: v1.PodSpec{NodeName: selected[2], Containers: []v1.Container{{
					Name: "client", Image: utils.AgnhostImage, Args: []string{"pause"},
				}}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			active := make(map[int]*v1.Pod)
			resize := func(indices []int) {
				wanted := make(map[int]bool)
				for _, index := range indices {
					wanted[index] = true
				}
				for index, pod := range active {
					if !wanted[index] {
						Expect(cs.CoreV1().Pods(ns.Name).Delete(ctx, pod.Name, metav1.DeleteOptions{
							Preconditions: &metav1.Preconditions{UID: &pod.UID},
						})).To(Succeed())
						delete(active, index)
					}
				}
				for _, index := range indices {
					if active[index] != nil {
						continue
					}
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("backend-%d", index), Namespace: ns.Name,
							Labels: map[string]string{"app": app["app"], egressLabel: egressName}},
						Spec: v1.PodSpec{NodeName: selected[index%2]},
					}
					for _, port := range []int{8080, 9090} {
						pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
							Name: fmt.Sprintf("backend-%d", port), Image: utils.AgnhostImage,
							Args: []string{"netexec", fmt.Sprintf("--http-port=%d", port), fmt.Sprintf("--udp-port=%d", port+1)},
							ReadinessProbe: &v1.Probe{ProbeHandler: v1.ProbeHandler{
								HTTPGet: &v1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(port)},
							}, PeriodSeconds: 2},
						})
					}
					created, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					active[index] = created
				}
			}
			nrms := make(map[string]map[string]nicNSGRule)
			associations := make(map[string]string)
			inboundPresent := true
			checkState := func(count int, sources string) []v1.Pod {
				var live []v1.Pod
				Eventually(func() error {
					pods, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(app).String()})
					if err != nil {
						return err
					}
					if len(pods.Items) != count {
						return fmt.Errorf("have %d backend Pods, want %d", len(pods.Items), count)
					}
					addresses := make(map[string][]string)
					for _, pod := range pods.Items {
						ready := false
						for _, condition := range pod.Status.Conditions {
							ready = ready || condition.Type == v1.PodReady && condition.Status == v1.ConditionTrue
						}
						if !ready || pod.DeletionTimestamp != nil {
							return fmt.Errorf("Pod %s is not a live Ready backend", pod.Name)
						}
						for _, address := range pod.Status.PodIPs {
							ip, err := netip.ParseAddr(address.IP)
							if err != nil {
								return err
							}
							if ip.Is6() == (family == v1.IPv6Protocol) {
								addresses[pod.Spec.NodeName] = append(addresses[pod.Spec.NodeName], ip.String())
							}
						}
					}
					want, err := inboundPodIPSet(cs, pods.Items, []v1.IPFamily{family})
					if err != nil {
						return err
					}
					if len(want) != count {
						return fmt.Errorf("have %d selected-family addresses, want %d", len(want), count)
					}
					if inboundPresent {
						if err := serviceReconciledMatchErr(uid, want); err != nil {
							return err
						}
					} else if err := serviceDeletedErr(uid); err != nil {
						return err
					}
					if count > 0 {
						if err := egressPodsRegisteredErr(cs, ns.Name, egressName, count); err != nil {
							return err
						}
					} else {
						if err := registeredAddressesMatchErr(outboundServiceName(egressName), map[string]struct{}{}); err != nil {
							return err
						}
						if err := natGatewayCleanupErr([]string{egressName}); err != nil {
							return err
						}
					}
					if inboundPresent && count > 0 && sources != "" {
						found, err := checkNICNSGPolicies(uid, sources, addresses, 8080)
						if err != nil {
							return err
						}
						for node, id := range found {
							if previous := associations[node]; previous != "" && !strings.EqualFold(previous, id) {
								return fmt.Errorf("managed NSG changed for live Node %s", node)
							}
							associations[node] = id
						}
					}
					groups, err := readNICNSGs()
					if err != nil {
						return err
					}
					currentNRMS := make(map[string]map[string]nicNSGRule)
					foundIDs := make(map[string]bool)
					for _, group := range groups {
						id := strings.ToLower(group.ID)
						foundIDs[id] = len(group.NICs) == 1
						if !inboundPresent || count == 0 || sources == "" {
							if len(nicNSGServiceRules(group, uid)) != 0 {
								return fmt.Errorf("obsolete Service rules remain on NSG %s", group.ID)
							}
						}
						currentNRMS[id] = make(map[string]nicNSGRule)
						for _, rule := range group.Rules {
							if strings.HasPrefix(strings.ToLower(rule.Name), "nrms-rule-") {
								currentNRMS[id][strings.ToLower(rule.Name)] = rule
							}
						}
					}
					for _, id := range associations {
						if !foundIDs[strings.ToLower(id)] {
							return fmt.Errorf("managed NSG %s or its NIC association disappeared", id)
						}
					}
					for id, rules := range nrms {
						for name, rule := range rules {
							if !reflect.DeepEqual(rule, currentNRMS[id][name]) {
								return fmt.Errorf("NRMS rule %s on %s changed or disappeared", name, id)
							}
						}
					}
					nrms, live = currentNRMS, pods.Items
					return nil
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
				assertAddressLocationFamilyPurity()
				return live
			}
			changeSources := func(sources string) {
				Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
					current, err := cs.CoreV1().Services(ns.Name).Get(ctx, service.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if current.Annotations == nil {
						current.Annotations = make(map[string]string)
					}
					current.Annotations[consts.ServiceAnnotationAllowedIPRanges] = sources
					_, err = cs.CoreV1().Services(ns.Name).Update(ctx, current, metav1.UpdateOptions{})
					return err
				})).To(Succeed())
			}
			podAddress := func(pod v1.Pod) string {
				for _, address := range pod.Status.PodIPs {
					ip, err := netip.ParseAddr(address.IP)
					Expect(err).NotTo(HaveOccurred())
					if ip.Is6() == (family == v1.IPv6Protocol) {
						return ip.String()
					}
				}
				Fail("backend lacks the selected address family")
				return ""
			}
			var vip string
			checkTraffic := func(pods []v1.Pod, allowed, useVIP bool) {
				names := make(map[string]struct{})
				for _, pod := range pods {
					names[pod.Name] = struct{}{}
				}
				probe := func() error {
					for _, pod := range pods {
						expected := map[string]struct{}{pod.Name: {}}
						for _, port := range []int{8080, 9090} {
							body, code, err := nsgScaleCurl(ns.Name, "client", "client", nsgScaleURL(podAddress(pod), port, "hostname"), family)
							if err != nil {
								return err
							}
							if err := nsgScaleResponseErr(body, code, expected, allowed || port == 9090); err != nil {
								return fmt.Errorf("Pod %s port %d: %w", pod.Name, port, err)
							}
						}
					}
					if useVIP {
						body, code, err := nsgScaleCurl(ns.Name, "client", "client", nsgScaleURL(vip, 80, "hostname"), family)
						if err != nil {
							return err
						}
						return nsgScaleResponseErr(body, code, names, allowed)
					}
					return nil
				}
				Eventually(probe, 2*time.Minute, defaultPollInterval).Should(Succeed())
				for range 3 {
					Expect(probe()).To(Succeed())
				}
				if allowed && useVIP {
					seen := make(map[string]struct{})
					Eventually(func() error {
						for range len(pods) * 2 {
							body, code, err := nsgScaleCurl(ns.Name, "client", "client", nsgScaleURL(vip, 80, "hostname"), family)
							if err != nil {
								return err
							}
							if err := nsgScaleResponseErr(body, code, names, true); err != nil {
								return err
							}
							seen[body] = struct{}{}
						}
						if len(seen) != len(names) {
							return fmt.Errorf("VIP reached %d of %d live backends", len(seen), len(names))
						}
						return nil
					}, 2*time.Minute, defaultPollInterval).Should(Succeed())
				}
			}
			checkOutbound := func(pods []v1.Pod) {
				services, err := queryServiceGatewayServices()
				Expect(err).NotTo(HaveOccurred())
				gateway, err := nsgScaleGatewayID(services.Value, egressName)
				Expect(err).NotTo(HaveOccurred())
				pips, err := getNatGatewayPublicIPs(gateway)
				Expect(err).NotTo(HaveOccurred())
				endpoint := "https://ifconfig.me/ip"
				if family == v1.IPv6Protocol {
					endpoint = "https://ifconfig.co/ip"
				}
				for _, pod := range pods {
					Eventually(func() error {
						body, code, err := nsgScaleCurl(ns.Name, pod.Name, "backend-8080", endpoint, family)
						if err != nil {
							return err
						}
						return nsgScaleSNATErr(body, code, pips, family)
					}, 2*time.Minute, defaultPollInterval).Should(Succeed(), "Pod %s must use its selected gateway", pod.Name)
				}
			}

			By("Establishing unrestricted inbound and explicit outbound baselines")
			resize([]int{0, 1})
			pods := checkState(2, "")
			Eventually(func() error {
				current, err := cs.CoreV1().Services(ns.Name).Get(ctx, service.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				for _, ingress := range current.Status.LoadBalancer.Ingress {
					ip, err := netip.ParseAddr(ingress.IP)
					if err == nil && ip.Is6() == (family == v1.IPv6Protocol) {
						vip = ip.String()
						return nil
					}
				}
				return fmt.Errorf("selected-family VIP not assigned")
			}, 5*time.Minute, defaultPollInterval).Should(Succeed())
			checkTraffic(pods, true, true)
			checkOutbound(pods)
			var prefixes []netip.Prefix
			targets := []string{nsgScaleURL(vip, 80, "clientip")}
			for _, pod := range pods {
				targets = append(targets, nsgScaleURL(podAddress(pod), 8080, "clientip"))
			}
			for _, target := range targets {
				body, code, err := nsgScaleCurl(ns.Name, "client", "client", target, family)
				Expect(err).NotTo(HaveOccurred())
				Expect(code).To(BeZero())
				ip, err := nsgScaleClientIP(body)
				Expect(err).NotTo(HaveOccurred())
				Expect(ip.Is6()).To(Equal(family == v1.IPv6Protocol))
				prefixes = append(prefixes, netip.PrefixFrom(ip, ip.BitLen()))
			}
			var sources []string
			for _, prefix := range iputil.AggregatePrefixes(prefixes) {
				sources = append(sources, prefix.String())
			}
			allowed := strings.Join(sources, ",")
			blocked := "192.0.2.0/24"
			if family == v1.IPv6Protocol {
				blocked = "2001:db8::/32"
			}
			for _, prefix := range prefixes {
				Expect(netip.MustParsePrefix(blocked).Contains(prefix.Addr())).To(BeFalse())
			}
			changeSources(allowed)
			for _, stage := range [][]int{{0, 1}, {0, 1, 2, 3, 4, 5}, {1, 3}, {}, {6, 7}} {
				By(fmt.Sprintf("Reconciling %d shared inbound/outbound backends", len(stage)))
				resize(stage)
				pods = checkState(len(stage), allowed)
				if len(stage) == 0 {
					continue
				}
				checkTraffic(pods, true, true)
				checkOutbound(pods)
				changeSources(blocked)
				pods = checkState(len(stage), blocked)
				checkTraffic(pods, false, true)
				checkOutbound(pods)
				changeSources(allowed)
				pods = checkState(len(stage), allowed)
				checkTraffic(pods, true, true)
				checkOutbound(pods)
			}
			By("Removing inbound while keeping the same Pods' outbound service working")
			Expect(cs.CoreV1().Services(ns.Name).Delete(ctx, service.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &service.UID},
			})).To(Succeed())
			inboundPresent = false
			pods = checkState(2, "")
			checkTraffic(pods, true, false)
			checkOutbound(pods)
			By("Removing the last egress Pods without deleting default resources or NRMS rules")
			resize(nil)
			checkState(0, "")
		},
		Entry("IPv4", v1.IPv4Protocol),
		Entry("IPv6", v1.IPv6Protocol),
	)

	It("should handle 10 services with varying pod counts (small, medium, large)", func() {
		const (
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)

		type serviceConfig struct {
			name     string
			podCount int
		}

		services := []serviceConfig{
			{"small-svc-1", 5},
			{"small-svc-2", 5},
			{"medium-svc-1", 10},
			{"medium-svc-2", 10},
			{"medium-svc-3", 10},
			{"large-svc-1", 15},
			{"large-svc-2", 15},
			{"xlarge-svc-1", 20},
			{"xlarge-svc-2", 15},
			{"mega-svc", 10},
		}

		totalPods := 0
		for _, svc := range services {
			totalPods += svc.podCount
		}

		utils.Logf("Creating mixed workload: 10 services with %d total pods", totalPods)
		Expect(totalPods).To(BeNumerically("<=", 120), "Total pods must not exceed 120")

		serviceUIDs := make(map[string]string)

		By("Creating all services and pods")
		for _, svcConfig := range services {
			serviceLabels := map[string]string{
				"app": svcConfig.name,
			}

			// Create service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcConfig.name,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
					Selector:              serviceLabels,
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}

			createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs[svcConfig.name] = string(createdService.UID)

			// Create pods for this service
			for i := 0; i < svcConfig.podCount; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", svcConfig.name, i),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			utils.Logf("Created service %s with %d pods", svcConfig.name, svcConfig.podCount)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d pods are ready", totalPods)

		By("Waiting for Azure to provision all resources")
		// Assert each service's exact address set. A per-service count cannot tell services apart:
		// registering service A's pod IPs under service B keeps every count correct while routing
		// traffic to the wrong workload.
		Eventually(func() error {
			for _, svcConfig := range services {
				want, podCount, err := livePodIPsFor(cs, ns.Name, svcConfig.name)
				if err != nil {
					return err
				}
				if podCount != svcConfig.podCount {
					return fmt.Errorf("service %s: expected %d Ready pods, got %d", svcConfig.name, svcConfig.podCount, podCount)
				}
				if err := serviceReconciledMatchErr(serviceUIDs[svcConfig.name], want); err != nil {
					return fmt.Errorf("service %s: %w", svcConfig.name, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"every service must register exactly its own pods' IPs in Azure and the Service Gateway")

		By("Verifying all services in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for svcName, uid := range serviceUIDs {
				if sgSvc.Name == inboundServiceName(uid) {
					foundServices++
					utils.Logf("Found service %s (UID: %s) in Service Gateway", svcName, uid)
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(services)), fmt.Sprintf("Expected %d services in Service Gateway, found %d", len(services), foundServices))

		By("Verifying each service registered exactly its own pod IPs")
		for svcName, uid := range serviceUIDs {
			want, podCount, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registeredAddressesMatchErr(inboundServiceName(uid), want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs", svcName)
			utils.Logf("Service %s: %d eligible IPs matched for %d Ready pods", svcName, len(want), podCount)
		}

		utils.Logf("\n✓ Mixed workload test passed: 10 services, %d total pods", totalPods)
	})

	It("should handle services with mixed traffic policies (Local vs Cluster)", func() {
		const (
			numServicesPerPolicy = 5
			podsPerService       = 10
			servicePort          = int32(8080)
			targetPort           = 8080
			waitTime             = 60 * time.Second
		)

		serviceUIDs := make(map[string]string)
		totalPods := numServicesPerPolicy * 2 * podsPerService

		utils.Logf("Creating %d services: %d with Local policy, %d with Cluster policy (%d total pods)",
			numServicesPerPolicy*2, numServicesPerPolicy, numServicesPerPolicy, totalPods)

		By("Creating services with Local traffic policy")
		for i := 0; i < numServicesPerPolicy; i++ {
			serviceName := fmt.Sprintf("local-policy-svc-%d", i)
			serviceLabels := map[string]string{
				"app":    serviceName,
				"policy": "local",
			}

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
					Selector:              serviceLabels,
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}

			createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs[serviceName] = string(createdService.UID)

			for j := 0; j < podsPerService; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Creating services with Cluster traffic policy")
		for i := 0; i < numServicesPerPolicy; i++ {
			serviceName := fmt.Sprintf("cluster-policy-svc-%d", i)
			serviceLabels := map[string]string{
				"app":    serviceName,
				"policy": "cluster",
			}

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
					Selector:              serviceLabels,
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}

			createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs[serviceName] = string(createdService.UID)

			for j := 0; j < podsPerService; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure provisioning")
		Eventually(func() error {
			for svcName, uid := range serviceUIDs {
				want, podCount, err := livePodIPsFor(cs, ns.Name, svcName)
				if err != nil {
					return err
				}
				if podCount != podsPerService {
					return fmt.Errorf("service %s: expected %d Ready pods, got %d", svcName, podsPerService, podCount)
				}
				if err := serviceReconciledMatchErr(uid, want); err != nil {
					return fmt.Errorf("service %s: %w", svcName, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all services should be reconciled in Azure and the Service Gateway")

		By("Verifying all services registered in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for svcName, uid := range serviceUIDs {
				if sgSvc.Name == inboundServiceName(uid) {
					foundServices++
					utils.Logf("Found service %s (UID: %s) in Service Gateway", svcName, uid)
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(serviceUIDs)), fmt.Sprintf("Expected %d services, found %d", len(serviceUIDs), foundServices))

		By("Verifying all pod IPs registered")
		for svcName, uid := range serviceUIDs {
			want, podCount, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registeredAddressesMatchErr(inboundServiceName(uid), want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs regardless of traffic policy", svcName)
			utils.Logf("Service %s: %d eligible IPs matched for %d Ready pods", svcName, len(want), podCount)
		}

		utils.Logf("\n✓ Mixed traffic policy test passed: %d services (%d Local, %d Cluster), %d total pods",
			len(serviceUIDs), numServicesPerPolicy, numServicesPerPolicy, totalPods)
	})

	It("should handle combined large-scale workload with 8 services and 120 total pods", func() {
		const (
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)

		type serviceConfig struct {
			name          string
			podCount      int
			trafficPolicy v1.ServiceExternalTrafficPolicyType
		}

		services := []serviceConfig{
			{"alpha-svc", 20, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"beta-svc", 15, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"gamma-svc", 18, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"delta-svc", 12, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"epsilon-svc", 20, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"zeta-svc", 15, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"eta-svc", 10, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"theta-svc", 10, v1.ServiceExternalTrafficPolicyTypeCluster},
		}

		totalPods := 0
		for _, svc := range services {
			totalPods += svc.podCount
		}

		utils.Logf("Creating combined workload: %d services with %d total pods", len(services), totalPods)
		Expect(totalPods).To(Equal(120), "Should use exactly 120 pods")

		serviceUIDs := make(map[string]string)

		By("Creating all services and pods")
		for _, svcConfig := range services {
			serviceLabels := map[string]string{
				"app": svcConfig.name,
			}

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcConfig.name,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: svcConfig.trafficPolicy,
					Selector:              serviceLabels,
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}

			createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs[svcConfig.name] = string(createdService.UID)

			for i := 0; i < svcConfig.podCount; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", svcConfig.name, i),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			utils.Logf("Created service %s (%s policy) with %d pods",
				svcConfig.name, svcConfig.trafficPolicy, svcConfig.podCount)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All 120 pods are ready")

		By("Waiting for Azure to provision all resources")
		Eventually(func() error {
			for _, svcConfig := range services {
				want, podCount, err := livePodIPsFor(cs, ns.Name, svcConfig.name)
				if err != nil {
					return err
				}
				if podCount != svcConfig.podCount {
					return fmt.Errorf("service %s: expected %d Ready pods, got %d", svcConfig.name, svcConfig.podCount, podCount)
				}
				if err := serviceReconciledMatchErr(serviceUIDs[svcConfig.name], want); err != nil {
					return fmt.Errorf("service %s: %w", svcConfig.name, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all services should be reconciled in Azure and the Service Gateway")

		By("Verifying all services in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for _, uid := range serviceUIDs {
				if sgSvc.Name == inboundServiceName(uid) {
					foundServices++
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(services)), fmt.Sprintf("Expected %d services, found %d", len(services), foundServices))

		By("Verifying eligible addresses for all 120 pods")
		totalRegisteredIPs := 0
		totalReadyPods := 0
		for svcName, uid := range serviceUIDs {
			expectedPods := 0
			for _, svcConfig := range services {
				if svcConfig.name == svcName {
					expectedPods = svcConfig.podCount
					break
				}
			}

			want, podCount, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(podCount).To(Equal(expectedPods), "service %s should have %d live pods", svcName, expectedPods)
			Expect(registeredAddressesMatchErr(inboundServiceName(uid), want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs", svcName)
			totalRegisteredIPs += len(want)
			totalReadyPods += podCount
		}

		Expect(totalReadyPods).To(Equal(120), "Should have exactly 120 Ready pods")
		utils.Logf("Verified %d eligible addresses for %d Ready pods", totalRegisteredIPs, totalReadyPods)

		utils.Logf("\n✓ Combined large-scale workload test passed: %d services, 120 pods", len(services))
	})
})

func nsgScaleURL(address string, port int, path string) string {
	return "http://" + net.JoinHostPort(address, strconv.Itoa(port)) + "/" + path
}

func nsgScaleCurl(namespace, pod, container, target string, family v1.IPFamily) (string, int, error) {
	flag := "-4"
	if family == v1.IPv6Protocol {
		flag = "-6"
	}
	script := `curl "$2" --silent --show-error --fail --no-keepalive --connect-timeout 2 --max-time 4 -H 'Connection: close' --write-out '\nSGW_CURL_META=%{num_connects},%{http_code}' "$1"; status=$?; printf '\nSGW_CURL_EXIT=%s\n' "$status"`
	stdout, stderr, err := utils.NewKubectlCommand(namespace, "--request-timeout=30s",
		"exec", pod, "-c", container, "--", "/bin/sh", "-c", script, "curl", target, flag).ExecWithFullOutput(false)
	if err != nil {
		return "", 0, fmt.Errorf("exec transport failed: %w; stderr: %s", err, stderr)
	}
	return nsgScaleCurlOutput(stdout)
}

func nsgScaleCurlOutput(output string) (string, int, error) {
	const (
		marker = "\nSGW_CURL_EXIT="
		meta   = "\nSGW_CURL_META="
	)
	if strings.Count(output, marker) != 1 || strings.Count(output, meta) != 1 {
		return "", 0, fmt.Errorf("missing or ambiguous curl completion marker")
	}
	response, status, _ := strings.Cut(output, marker)
	code, err := strconv.Atoi(strings.TrimSpace(status))
	if err != nil || code < 0 || code > 255 {
		return "", 0, fmt.Errorf("invalid curl completion status %q", status)
	}
	body, stats, _ := strings.Cut(response, meta)
	connectionText, httpText, found := strings.Cut(strings.TrimSpace(stats), ",")
	connections, connectionErr := strconv.Atoi(connectionText)
	httpCode, httpErr := strconv.Atoi(httpText)
	if !found || connectionErr != nil || httpErr != nil || connections < 0 || httpCode < 0 || httpCode > 599 {
		return "", 0, fmt.Errorf("invalid curl connection metadata %q", stats)
	}
	// Curl timeouts after connecting are not evidence of a denied connection.
	if (code == 7 || code == 28) && (connections != 0 || httpCode != 0) {
		return "", 0, fmt.Errorf("curl exited %d after connecting or receiving an HTTP response", code)
	}
	return strings.TrimSpace(body), code, nil
}

func nsgScaleResponseErr(body string, code int, names map[string]struct{}, allowed bool) error {
	if !allowed {
		if (code == 7 || code == 28) && body == "" {
			return nil
		}
		return fmt.Errorf("expected a connection denial, got curl exit %d and body %q", code, body)
	}
	if _, expected := names[body]; code != 0 || !expected {
		return fmt.Errorf("expected a live backend response, got curl exit %d and body %q", code, body)
	}
	return nil
}

func nsgScaleClientIP(body string) (netip.Addr, error) {
	if address, err := netip.ParseAddrPort(body); err == nil {
		return address.Addr().Unmap(), nil
	}
	address, err := netip.ParseAddr(body)
	return address.Unmap(), err
}

func nsgScaleSNATErr(body string, code int, publicIPs []string, family v1.IPFamily) error {
	address, err := netip.ParseAddr(body)
	if err != nil || code != 0 || address.Unmap().Is6() != (family == v1.IPv6Protocol) {
		return fmt.Errorf("outbound source is not an observed %s address: curl exit %d, body %q", family, code, body)
	}
	for _, value := range publicIPs {
		if candidate, err := netip.ParseAddr(value); err == nil && candidate.Unmap() == address.Unmap() {
			return nil
		}
	}
	return fmt.Errorf("observed outbound source %s is not a selected NAT gateway public IP", address)
}

func nsgScaleGatewayID(services []ServiceGatewayService, name string) (string, error) {
	serviceName := outboundServiceName(name)
	expected := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
		subscriptionID, resourceGroupName, serviceName)
	for _, service := range services {
		if service.Name == serviceName && service.Properties.ServiceType == "Outbound" {
			if !strings.EqualFold(service.Properties.PublicNatGatewayID, expected) {
				return "", fmt.Errorf("egress %s is linked to %q instead of requested managed gateway %q",
					name, service.Properties.PublicNatGatewayID, expected)
			}
			return expected, nil
		}
	}
	return "", fmt.Errorf("outbound service %s is missing", name)
}

func TestNSGScaleGatewayIdentity(t *testing.T) {
	oldSubscription, oldGroup := subscriptionID, resourceGroupName
	subscriptionID, resourceGroupName = "network-sub", "network-rg"
	t.Cleanup(func() { subscriptionID, resourceGroupName = oldSubscription, oldGroup })
	expected := "/subscriptions/network-sub/resourceGroups/network-rg/providers/Microsoft.Network/natGateways/requested-natgw"
	for _, tt := range []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"requested gateway", expected, false},
		{"ARM ID casing", strings.ToUpper(expected), false},
		{"default gateway substitution", strings.Replace(expected, "/requested-natgw", "/default-natgw", 1), true},
		{"legacy unsuffixed gateway", strings.TrimSuffix(expected, "-natgw"), true},
		{"wrong resource group", strings.Replace(expected, "network-rg", "another-rg", 1), true},
		{"wrong subscription", strings.Replace(expected, "network-sub", "another-sub", 1), true},
		{"missing target", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service := ServiceGatewayService{Name: "requested-natgw"}
			service.Properties.ServiceType, service.Properties.PublicNatGatewayID = "Outbound", tt.id
			got, err := nsgScaleGatewayID([]ServiceGatewayService{service}, "requested")
			g := NewWithT(t)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			if !tt.wantErr {
				g.Expect(got).To(Equal(expected))
			}
		})
	}
	_, err := nsgScaleGatewayID(nil, "requested")
	NewWithT(t).Expect(err).To(HaveOccurred())
	legacy := ServiceGatewayService{Name: "requested"}
	legacy.Properties.ServiceType, legacy.Properties.PublicNatGatewayID = "Outbound", expected
	_, err = nsgScaleGatewayID([]ServiceGatewayService{legacy}, "requested")
	NewWithT(t).Expect(err).To(HaveOccurred())
}

func TestNSGScaleTrafficResponses(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        string
		code        int
		connections int
		httpCode    int
		allowed     bool
		wantErr     bool
	}{
		{"allowed backend", "backend-0", 0, 1, 200, true, false},
		{"wrong backend", "deleted-backend", 0, 1, 200, true, true},
		{"missing backend identity", "", 0, 1, 200, true, true},
		{"allowed timeout", "", 28, 0, 0, true, true},
		{"denied timeout", "", 28, 0, 0, false, false},
		{"timeout after response data", "backend-0", 28, 1, 200, false, true},
		{"timeout after TCP connect", "", 28, 1, 0, false, true},
		{"timeout after HTTP headers", "", 28, 0, 200, false, true},
		{"response bytes despite empty stats", "backend-0", 28, 0, 0, false, true},
		{"denied connection refusal", "", 7, 0, 0, false, false},
		{"unexpected allowed traffic", "backend-0", 0, 1, 200, false, true},
		{"HTTP failure is not denial", "", 22, 1, 403, false, true},
		{"TLS failure is not denial", "", 60, 1, 0, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := fmt.Sprintf("%s\nSGW_CURL_META=%d,%03d\nSGW_CURL_EXIT=%d\n", tt.body, tt.connections, tt.httpCode, tt.code)
			body, code, err := nsgScaleCurlOutput(output)
			if err == nil {
				err = nsgScaleResponseErr(body, code, map[string]struct{}{"backend-0": {}}, tt.allowed)
			}
			NewWithT(t).Expect(err != nil).To(Equal(tt.wantErr))
		})
	}
	for _, output := range []string{
		"error dialing backend: 503",
		"\nSGW_CURL_EXIT=28\n",
		"\nSGW_CURL_META=0,000\nSGW_CURL_EXIT=error\n",
		"\nSGW_CURL_META=0,000\nSGW_CURL_EXIT=0\nSGW_CURL_EXIT=28\n",
		"\nSGW_CURL_META=invalid,000\nSGW_CURL_EXIT=28\n",
		"\nSGW_CURL_META=-1,000\nSGW_CURL_EXIT=28\n",
	} {
		_, _, err := nsgScaleCurlOutput(output)
		NewWithT(t).Expect(err).To(HaveOccurred())
	}
}

func TestNSGScaleObservedSources(t *testing.T) {
	for _, value := range []string{"203.0.113.1:1234", "[2001:db8::1]:1234", "203.0.113.1", "::ffff:203.0.113.1"} {
		_, err := nsgScaleClientIP(value)
		NewWithT(t).Expect(err).NotTo(HaveOccurred())
	}
	for _, value := range []string{"", "203.0.113.1:bad", "error dialing 203.0.113.1:443", "[2001:db8::1]:bad"} {
		_, err := nsgScaleClientIP(value)
		NewWithT(t).Expect(err).To(HaveOccurred())
	}
	pips := []string{"203.0.113.1", "2001:db8::1"}
	for _, tt := range []struct {
		name    string
		body    string
		code    int
		family  v1.IPFamily
		wantErr bool
	}{
		{"IPv4 SNAT", "203.0.113.1", 0, v1.IPv4Protocol, false},
		{"IPv6 SNAT", "2001:DB8::1", 0, v1.IPv6Protocol, false},
		{"wrong gateway", "203.0.113.2", 0, v1.IPv4Protocol, true},
		{"wrong family", "203.0.113.1", 0, v1.IPv6Protocol, true},
		{"missing source", "", 0, v1.IPv4Protocol, true},
		{"request failure", "203.0.113.1", 22, v1.IPv4Protocol, true},
		{"address in error text", "failed connecting to 203.0.113.1", 0, v1.IPv4Protocol, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := nsgScaleSNATErr(tt.body, tt.code, pips, tt.family)
			NewWithT(t).Expect(err != nil).To(Equal(tt.wantErr))
		})
	}
}

func TestNSGScaleExecFailureIsNotTrafficDenial(t *testing.T) {
	directory := t.TempDir()
	config := filepath.Join(directory, "kubeconfig")
	g := NewWithT(t)
	g.Expect(os.WriteFile(config, []byte("apiVersion: v1\nkind: Config\n"), 0o600)).To(Succeed())
	// #nosec G306 -- owner-only executable kubectl fixture.
	g.Expect(os.WriteFile(filepath.Join(directory, "kubectl"), []byte("#!/bin/sh\nprintf '\\nSGW_CURL_META=0,000\\nSGW_CURL_EXIT=28\\n'\nexit 1\n"), 0o700)).To(Succeed())
	t.Setenv("KUBECONFIG", config)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, err := nsgScaleCurl("test", "client", "client", "http://10.0.0.1:8080/hostname", v1.IPv4Protocol)
	g.Expect(err).To(MatchError(ContainSubstring("exec transport failed")))
}

// livePodIPsFor returns the address set the given service's pods should have registered. Specs
// compare against this set rather than a per-service count, which cannot detect one service's pods
// being registered under another service's name.
func livePodIPsFor(cs clientset.Interface, namespace, serviceName string) (map[string]struct{}, int, error) {
	service, err := cs.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, 0, err
	}
	return livePodIPsForSelector(cs, namespace, service.Spec.Selector, service.Spec.IPFamilies)
}
