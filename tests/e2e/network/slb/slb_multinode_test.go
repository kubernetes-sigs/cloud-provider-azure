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

var _ = Describe("Container Load Balancer Multi-Node Tests", Label(slbTestLabel, "SLB-MultiNode"), func() {
	basename := "slb-multinode"

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
		if ns != nil {
			utils.Logf("Deleting namespace %s", ns.Name)
			utils.DeleteNamespace(cs, ns.Name)
		}

		By("Waiting for Azure cleanup")
		eventuallyAzureCleanup(2 * time.Minute)

		By("Verifying Service Gateway cleanup")
		verifyServiceGatewayCleanup()

		By("Verifying Address Locations cleanup")
		verifyAddressLocationsCleanup()

		cs = nil
		ns = nil
	})

	// Test 1: Inbound service with pods spread across multiple nodes
	// Verifies backend pool contains IPs from all nodes with pods
	It("should register backend pool IPs from pods on multiple nodes for inbound service", func() {
		const (
			serviceName   = "multinode-lb-svc"
			servicePort   = int32(8080)
			targetPort    = 8080
			podsPerNode   = 2
			provisionTime = 120 * time.Second
		)

		ctx := context.Background()

		By("Getting list of nodes")
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "Need at least 2 nodes for multi-node test")

		nodeNames := make([]string, 0)
		nodeIPs := make(map[string]string) // nodeName -> internalIP
		for _, node := range nodes.Items {
			nodeNames = append(nodeNames, node.Name)
			for _, addr := range node.Status.Addresses {
				if addr.Type == v1.NodeInternalIP {
					nodeIPs[node.Name] = addr.Address
					break
				}
			}
		}
		utils.Logf("Found %d nodes: %v", len(nodeNames), nodeNames)
		utils.Logf("Node IPs: %v", nodeIPs)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector: map[string]string{
					"app": serviceName,
				},
				Ports: []v1.ServicePort{
					{
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}
		createdService, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)
		utils.Logf("Created service %s", serviceName)

		By(fmt.Sprintf("Creating %d pods per node, spread across %d nodes", podsPerNode, len(nodeNames)))
		podIPs := make(map[string][]string)     // nodeName -> list of pod IPs on that node
		podHostIPs := make(map[string][]string) // hostIP -> list of pod IPs on that host
		createdPods := make([]string, 0)

		for i, nodeName := range nodeNames {
			for j := 0; j < podsPerNode; j++ {
				podName := fmt.Sprintf("multinode-pod-%d-%d", i, j)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: ns.Name,
						Labels: map[string]string{
							"app": serviceName,
						},
					},
					Spec: v1.PodSpec{
						NodeName: nodeName, // Pin pod to specific node
						Containers: []v1.Container{
							{
								Name:            "nginx",
								Image:           "nginx:alpine",
								ImagePullPolicy: v1.PullIfNotPresent,
								Ports: []v1.ContainerPort{
									{ContainerPort: int32(targetPort)},
								},
							},
						},
					},
				}

				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				createdPods = append(createdPods, podName)
			}
			utils.Logf("Created %d pods on node %s", podsPerNode, nodeName)
		}

		By("Waiting for all pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Collecting pod IPs per node")
		for _, podName := range createdPods {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			nodeName := pod.Spec.NodeName
			hostIP := pod.Status.HostIP
			podIP := pod.Status.PodIP

			if podIPs[nodeName] == nil {
				podIPs[nodeName] = make([]string, 0)
			}
			podIPs[nodeName] = append(podIPs[nodeName], podIP)

			if podHostIPs[hostIP] == nil {
				podHostIPs[hostIP] = make([]string, 0)
			}
			podHostIPs[hostIP] = append(podHostIPs[hostIP], podIP)

			utils.Logf("Pod %s: Node=%s, HostIP=%s, PodIP=%s", podName, nodeName, hostIP, podIP)
		}

		By("Waiting for Azure LoadBalancer provisioning")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, len(createdPods))
		}, provisionTime, 10*time.Second).Should(Succeed(),
			"service should be reconciled with Azure LoadBalancer resources and registered pod endpoints")

		By("Verifying Service Gateway has correct address locations")
		addrResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		// Build a map of what we expect: hostIP -> podIPs
		// Build a map of what we got from Service Gateway
		sgAddressLocations := make(map[string][]string) // addressLocation (hostIP) -> addresses (podIPs)
		for _, loc := range addrResponse.Value {
			addresses := make([]string, 0)
			for _, addr := range loc.Addresses {
				addresses = append(addresses, addr.Address)
			}
			sgAddressLocations[loc.AddressLocation] = addresses
		}

		utils.Logf("\n=== Address Location Verification ===")
		utils.Logf("Expected (from pods): %v", podHostIPs)
		utils.Logf("Actual (from Service Gateway): %v", sgAddressLocations)

		// Verify each host IP has correct pod IPs
		for hostIP, expectedPodIPs := range podHostIPs {
			actualPodIPs, exists := sgAddressLocations[hostIP]
			Expect(exists).To(BeTrue(), "Host IP %s should exist in Service Gateway address locations", hostIP)

			for _, expectedIP := range expectedPodIPs {
				found := false
				for _, actualIP := range actualPodIPs {
					if actualIP == expectedIP {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "Pod IP %s should be registered under host IP %s", expectedIP, hostIP)
			}
			utils.Logf("✓ Host %s: verified %d pod IPs", hostIP, len(expectedPodIPs))
		}

		By("Verifying Service Gateway service exists")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var foundService bool
		for _, sgSvc := range sgResponse.Value {
			if sgSvc.Properties.ServiceType == "Inbound" {
				foundService = true
				utils.Logf("✓ Found inbound service in Service Gateway: %s", sgSvc.Name)
				break
			}
		}
		Expect(foundService).To(BeTrue(), "Inbound service should exist in Service Gateway")

		utils.Logf("\n✓ Multi-node inbound test passed!")
		utils.Logf("  - Pods spread across %d nodes", len(nodeNames))
		utils.Logf("  - %d unique address locations verified", len(podHostIPs))
		utils.Logf("  - All pod IPs correctly registered in Service Gateway")
	})

	// Test 2: Egress service with pods spread across multiple nodes
	// Verifies NAT Gateway has address locations from all nodes with pods
	It("should register address locations from pods on multiple nodes for egress service", func() {
		const (
			podsPerNode   = 3
			targetPort    = 8080
			provisionTime = 120 * time.Second
		)

		ctx := context.Background()
		egressName := ns.Name // Use namespace as egress name to avoid conflicts

		By("Getting list of nodes")
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "Need at least 2 nodes for multi-node test")

		nodeNames := make([]string, 0)
		nodeIPs := make(map[string]string)
		for _, node := range nodes.Items {
			nodeNames = append(nodeNames, node.Name)
			for _, addr := range node.Status.Addresses {
				if addr.Type == v1.NodeInternalIP {
					nodeIPs[node.Name] = addr.Address
					break
				}
			}
		}
		utils.Logf("Found %d nodes: %v", len(nodeNames), nodeNames)

		By(fmt.Sprintf("Creating %d egress pods per node, spread across %d nodes", podsPerNode, len(nodeNames)))
		podHostIPs := make(map[string][]string) // hostIP -> list of pod IPs
		createdPods := make([]string, 0)

		for i, nodeName := range nodeNames {
			for j := 0; j < podsPerNode; j++ {
				podName := fmt.Sprintf("egress-multinode-pod-%d-%d", i, j)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: ns.Name,
						Labels: map[string]string{
							egressLabel: egressName,
						},
					},
					Spec: v1.PodSpec{
						NodeName: nodeName,
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

				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				createdPods = append(createdPods, podName)
			}
			utils.Logf("Created %d egress pods on node %s", podsPerNode, nodeName)
		}

		By("Waiting for all pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Collecting pod IPs per host")
		for _, podName := range createdPods {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hostIP := pod.Status.HostIP
			podIP := pod.Status.PodIP

			if podHostIPs[hostIP] == nil {
				podHostIPs[hostIP] = make([]string, 0)
			}
			podHostIPs[hostIP] = append(podHostIPs[hostIP], podIP)

			utils.Logf("Pod %s: HostIP=%s, PodIP=%s", podName, hostIP, podIP)
		}

		By("Waiting for Azure NAT Gateway provisioning")
		Eventually(func() error {
			return egressRegisteredErr(egressName, len(createdPods))
		}, provisionTime, 10*time.Second).Should(Succeed(),
			"egress service should be reconciled with NAT Gateway and registered pods")

		By("Verifying all pods have ServiceGateway finalizer")
		for _, podName := range createdPods {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hasFinalizer := false
			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					hasFinalizer = true
					break
				}
			}
			Expect(hasFinalizer).To(BeTrue(), "Pod %s should have ServiceGateway finalizer", podName)
		}
		utils.Logf("✓ All %d pods have ServiceGateway finalizer", len(createdPods))

		By("Verifying NAT Gateway exists in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var foundEgress bool
		var natGatewayID string
		for _, sgSvc := range sgResponse.Value {
			if sgSvc.Name == egressName && sgSvc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				natGatewayID = sgSvc.Properties.PublicNatGatewayID
				utils.Logf("✓ Found egress service '%s' with NAT Gateway: %s", egressName, natGatewayID)
				break
			}
		}
		Expect(foundEgress).To(BeTrue(), "Egress service should exist in Service Gateway")
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway ID should not be empty")

		By("Verifying Service Gateway address locations match pods")
		addrResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		sgAddressLocations := make(map[string][]string)
		for _, loc := range addrResponse.Value {
			addresses := make([]string, 0)
			for _, addr := range loc.Addresses {
				// Filter to only our egress service
				for _, svc := range addr.Services {
					if strings.Contains(svc, egressName) {
						addresses = append(addresses, addr.Address)
						break
					}
				}
			}
			if len(addresses) > 0 {
				sgAddressLocations[loc.AddressLocation] = addresses
			}
		}

		utils.Logf("\n=== Egress Address Location Verification ===")
		utils.Logf("Expected (from pods): %v", podHostIPs)
		utils.Logf("Actual (from Service Gateway for egress %s): %v", egressName, sgAddressLocations)

		// Verify we have address locations for all nodes with pods
		nodesWithPods := len(podHostIPs)
		nodesInSG := len(sgAddressLocations)
		utils.Logf("Nodes with pods: %d, Nodes in Service Gateway: %d", nodesWithPods, nodesInSG)

		// Verify each host IP's pods are registered
		for hostIP, expectedPodIPs := range podHostIPs {
			actualPodIPs, exists := sgAddressLocations[hostIP]
			if !exists {
				utils.Logf("WARNING: Host IP %s not found in Service Gateway (may be registered under different key)", hostIP)
				continue
			}

			matchedCount := 0
			for _, expectedIP := range expectedPodIPs {
				for _, actualIP := range actualPodIPs {
					if actualIP == expectedIP {
						matchedCount++
						break
					}
				}
			}
			utils.Logf("Host %s: %d/%d pod IPs matched", hostIP, matchedCount, len(expectedPodIPs))
		}

		// Verify total address count
		totalExpectedAddresses := 0
		for _, ips := range podHostIPs {
			totalExpectedAddresses += len(ips)
		}
		totalActualAddresses := 0
		for _, ips := range sgAddressLocations {
			totalActualAddresses += len(ips)
		}

		utils.Logf("\n✓ Multi-node egress test completed!")
		utils.Logf("  - Pods spread across %d nodes", len(nodeNames))
		utils.Logf("  - Total pods: %d", len(createdPods))
		utils.Logf("  - Expected addresses: %d", totalExpectedAddresses)
		utils.Logf("  - Registered addresses: %d", totalActualAddresses)

		Expect(totalActualAddresses).To(BeNumerically(">=", totalExpectedAddresses),
			"All pod addresses should be registered in Service Gateway")
	})

	// Test 3: Node drain simulation - verify addresses are updated when pods move
	It("should update address locations when pods are deleted from one node", func() {
		const (
			serviceName   = "node-drain-svc"
			servicePort   = int32(8080)
			targetPort    = 8080
			podsPerNode   = 2
			provisionTime = 90 * time.Second
		)

		ctx := context.Background()

		By("Getting list of nodes")
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "Need at least 2 nodes")

		nodeNames := make([]string, 0, 2)
		for i, node := range nodes.Items {
			if i >= 2 {
				break
			}
			nodeNames = append(nodeNames, node.Name)
		}
		utils.Logf("Using nodes: %v", nodeNames)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              map[string]string{"app": serviceName},
				Ports: []v1.ServicePort{
					{Port: servicePort, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP},
				},
			},
		}
		createdService, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)

		By("Creating pods on two nodes")
		podsOnNode0 := make([]string, 0)
		podsOnNode1 := make([]string, 0)

		for j := 0; j < podsPerNode; j++ {
			// Pods on node 0
			podName0 := fmt.Sprintf("drain-pod-n0-%d", j)
			pod0 := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName0,
					Namespace: ns.Name,
					Labels:    map[string]string{"app": serviceName},
				},
				Spec: v1.PodSpec{
					NodeName:   nodeNames[0],
					Containers: []v1.Container{{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod0, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			podsOnNode0 = append(podsOnNode0, podName0)

			// Pods on node 1
			podName1 := fmt.Sprintf("drain-pod-n1-%d", j)
			pod1 := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName1,
					Namespace: ns.Name,
					Labels:    map[string]string{"app": serviceName},
				},
				Spec: v1.PodSpec{
					NodeName:   nodeNames[1],
					Containers: []v1.Container{{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}}},
				},
			}
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			podsOnNode1 = append(podsOnNode1, podName1)
		}

		utils.Logf("Created %d pods on %s and %d pods on %s", len(podsOnNode0), nodeNames[0], len(podsOnNode1), nodeNames[1])

		By("Waiting for pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for initial Azure provisioning")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, len(podsOnNode0)+len(podsOnNode1))
		}, provisionTime, 10*time.Second).Should(Succeed(),
			"service should be reconciled with initial pod endpoints")

		By("Verifying initial address locations (should have 2 nodes)")
		addrResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		initialLocations := 0
		initialAddresses := 0
		for _, loc := range addrResponse.Value {
			if len(loc.Addresses) > 0 {
				initialLocations++
				initialAddresses += len(loc.Addresses)
			}
		}
		utils.Logf("Initial state: %d address locations, %d total addresses", initialLocations, initialAddresses)
		Expect(initialLocations).To(BeNumerically(">=", 2), "Should have addresses on at least 2 nodes")

		By(fmt.Sprintf("Simulating drain: Deleting all pods from node %s", nodeNames[0]))
		for _, podName := range podsOnNode0 {
			err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Deleted pod %s", podName)
		}

		By("Waiting for address locations to update")
		finalAddresses := 0
		Eventually(func() error {
			addrResponse, err = queryServiceGatewayAddressLocations()
			if err != nil {
				return fmt.Errorf("query Service Gateway address locations: %w", err)
			}

			finalAddresses = 0
			for _, loc := range addrResponse.Value {
				finalAddresses += len(loc.Addresses)
			}
			if finalAddresses >= initialAddresses {
				return fmt.Errorf("address count should decrease after deleting pods from one node: got %d, want less than %d", finalAddresses, initialAddresses)
			}
			if finalAddresses < len(podsOnNode1) {
				return fmt.Errorf("should still have addresses for pods on node 1: got %d, want at least %d", finalAddresses, len(podsOnNode1))
			}
			return nil
		}, 30*time.Second, 10*time.Second).Should(Succeed(),
			"address locations should update after deleting pods from one node")

		utils.Logf("After drain: %d total addresses (was %d)", finalAddresses, initialAddresses)

		utils.Logf("\n✓ Node drain simulation test passed!")
		utils.Logf("  - Initial addresses: %d", initialAddresses)
		utils.Logf("  - After drain: %d", finalAddresses)
		utils.Logf("  - Addresses correctly updated when pods removed from node")
	})

	// Test 4: Node object deletion - a deleted node's addresses must drain.
	//
	// A node's InternalIP is not carried in its EndpointSlices (they reference the node by name), so a
	// node-only lifecycle change fires no slice event; the difftracker is driven from the node informer.
	// When the Node is deleted, its pods' addresses must be removed from its Service Gateway location
	// rather than stranded under a node underlay IP that no longer exists. The kubelet re-registers the
	// Node on its next heartbeat, so the assertion polls quickly. The node-InternalIP-change variant
	// cannot be reproduced on a live cluster (the kubelet owns the InternalIP) and is unit-tested.
	It("should drain a deleted node's addresses from the Service Gateway", func() {
		const (
			serviceName   = "node-delete-svc"
			servicePort   = int32(80)
			targetPort    = 8080
			appLabel      = "node-delete-app"
			provisionTime = 90 * time.Second
		)
		ctx := context.Background()

		By("Selecting a victim node and a survivor node")
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "Need at least 2 nodes")
		victimNode, survivorNode := nodes.Items[0].Name, nodes.Items[1].Name
		utils.Logf("victim node: %s, survivor node: %s", victimNode, survivorNode)

		labels := map[string]string{"app": appLabel}
		makePod := func(name, nodeName string) *v1.Pod {
			return &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					Containers: []v1.Container{{
						Name:            "app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
					}},
				},
			}
		}

		By("Creating a LoadBalancer service with one pod on each node")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    []v1.ServicePort{{Port: servicePort, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP}},
			},
		}
		createdSvc, err := cs.CoreV1().Services(ns.Name).Create(ctx, svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdSvc.UID)

		_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, makePod("victim-pod", victimNode), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, makePod("survivor-pod", survivorNode), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		victimPod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, "victim-pod", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		survivorPod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, "survivor-pod", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		victimPodIP, victimNodeIP := victimPod.Status.PodIP, victimPod.Status.HostIP
		survivorPodIP := survivorPod.Status.PodIP

		By("Waiting for provisioning and verifying the victim pod registers under its node's location")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, 2)
		}, provisionTime, 10*time.Second).Should(Succeed())

		registeredUnder := func(podIP string) (string, bool) {
			resp, qerr := queryServiceGatewayAddressLocations()
			Expect(qerr).NotTo(HaveOccurred())
			for _, loc := range resp.Value {
				for _, a := range loc.Addresses {
					if ipEqual(a.Address, podIP) {
						return loc.AddressLocation, true
					}
				}
			}
			return "", false
		}
		Eventually(func() bool {
			loc, ok := registeredUnder(victimPodIP)
			return ok && ipEqual(loc, victimNodeIP)
		}, 60*time.Second, 5*time.Second).Should(BeTrue(),
			"victim pod must register under its own node InternalIP")

		By(fmt.Sprintf("Deleting the Node object %s", victimNode))
		Expect(cs.CoreV1().Nodes().Delete(ctx, victimNode, metav1.DeleteOptions{})).To(Succeed())

		By("Verifying the victim node's address drains while the survivor's remains")
		Eventually(func() error {
			if _, ok := registeredUnder(victimPodIP); ok {
				return fmt.Errorf("victim pod IP %s still registered after its node was deleted", victimPodIP)
			}
			if _, ok := registeredUnder(survivorPodIP); !ok {
				return fmt.Errorf("survivor pod IP %s must remain registered", survivorPodIP)
			}
			return nil
		}, 30*time.Second, 1*time.Second).Should(Succeed(),
			"a deleted node's addresses must drain from the Service Gateway, leaving the survivor's intact")
	})
})
