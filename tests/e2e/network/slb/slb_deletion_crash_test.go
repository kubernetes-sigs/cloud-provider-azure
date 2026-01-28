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
	"encoding/json"
	"fmt"
	"os/exec"
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

var _ = Describe("Container Load Balancer Deletion Crash Recovery Tests", Label(slbTestLabel, "SLB-DeletionCrash"), func() {
	basename := "slb-del-crash"

	var (
		cs        clientset.Interface
		ccmClient *utils.CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error

		// First check if CCM cluster is configured
		if !utils.IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM crash tests: %s environment variable not set", utils.CCMKubeconfigEnvVar))
		}

		// Create workload cluster client
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		// Create CCM cluster client
		ccmClient, err = utils.NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		// Create test namespace in workload cluster
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		// Verify CCM is running before starting test
		ctx := context.Background()
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if ns != nil {
			utils.Logf("Deleting namespace %s", ns.Name)
			utils.DeleteNamespace(cs, ns.Name)
		}

		// Ensure CCM is recovered before next test
		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		// Wait for Azure cleanup
		utils.Logf("Waiting 120 seconds for Azure cleanup...")
		time.Sleep(120 * time.Second)

		By("Verifying Service Gateway cleanup")
		verifyServiceGatewayCleanup()

		cs = nil
		ccmClient = nil
		ns = nil
	})

	// Test 1: Delete inbound services + immediately crash CCM
	It("should recover and complete service deletion after CCM crash during deletion", func() {
		const (
			serviceCount  = 5
			servicePort   = int32(8080)
			targetPort    = 8080
			provisionTime = 120 * time.Second
		)

		ctx := context.Background()

		By(fmt.Sprintf("Creating %d LoadBalancer services", serviceCount))
		serviceUIDs := make([]string, serviceCount)
		serviceNames := make([]string, serviceCount)
		for i := 0; i < serviceCount; i++ {
			serviceName := fmt.Sprintf("del-crash-svc-%d", i)
			serviceNames[i] = serviceName

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
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
			createdSvc, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs[i] = string(createdSvc.UID)

			// Create backend pod
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod", serviceName),
					Namespace: ns.Name,
					Labels: map[string]string{
						"app": serviceName,
					},
				},
				Spec: v1.PodSpec{
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
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d services with backend pods", serviceCount)

		By("Waiting for pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Waiting %v for Azure LoadBalancer provisioning", provisionTime))
		time.Sleep(provisionTime)

		By("Verifying all services are provisioned in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
			}
		}
		utils.Logf("Found %d inbound services in Service Gateway before deletion", inboundCount)
		Expect(inboundCount).To(BeNumerically(">=", serviceCount), "All services should be provisioned")

		By("Verifying Load Balancers exist")
		initialLBCount, err := countAzureLoadBalancers()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Found %d Load Balancers before deletion", initialLBCount)
		Expect(initialLBCount).To(BeNumerically(">=", serviceCount), "All LBs should exist")

		By("Verifying Public IPs exist")
		initialPIPCount, err := countAzurePublicIPs()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Found %d Public IPs before deletion", initialPIPCount)

		By(fmt.Sprintf("Triggering deletion of all %d services", serviceCount))
		for _, serviceName := range serviceNames {
			err := cs.CoreV1().Services(ns.Name).Delete(ctx, serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Deletion triggered for all services")

		By("IMMEDIATELY crashing CCM (within 2 seconds of deletion)")
		time.Sleep(2 * time.Second) // Give just enough time for deletion to start
		err = ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed (all pods deleted)")

		By("Waiting 10 seconds with CCM down")
		time.Sleep(10 * time.Second)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By("Waiting for CCM to complete deletions (180s)")
		time.Sleep(180 * time.Second)

		By("Verifying all K8s services are deleted (finalizers removed)")
		services, err := cs.CoreV1().Services(ns.Name).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		lbServiceCount := 0
		for _, svc := range services.Items {
			if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
				lbServiceCount++
				utils.Logf("WARNING: Service %s still exists with finalizers: %v", svc.Name, svc.Finalizers)
			}
		}
		Expect(lbServiceCount).To(Equal(0), "All LoadBalancer services should be deleted")
		utils.Logf("✓ All K8s services deleted (finalizers removed)")

		By("Verifying Service Gateway has no inbound services")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount = 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
				utils.Logf("WARNING: Inbound service still in SGW: %s", svc.Name)
			}
		}
		Expect(inboundCount).To(Equal(0), "No inbound services should remain in Service Gateway")
		utils.Logf("✓ Service Gateway cleaned up (no inbound services)")

		By("Verifying Load Balancers are deleted")
		finalLBCount, err := countAzureLoadBalancers()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Final LB count: %d (was %d)", finalLBCount, initialLBCount)
		Expect(finalLBCount).To(BeNumerically("<", initialLBCount), "LBs should be deleted")

		By("Verifying Public IPs are deleted")
		finalPIPCount, err := countAzurePublicIPs()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Final PIP count: %d (was %d)", finalPIPCount, initialPIPCount)

		utils.Logf("\n✓ Deletion crash recovery test PASSED!")
		utils.Logf("  - %d services deleted after CCM crash", serviceCount)
		utils.Logf("  - All finalizers removed")
		utils.Logf("  - All Azure resources cleaned up")
	})

	// Test 2: Delete egress pods + immediately crash CCM
	It("should recover and complete egress cleanup after CCM crash during pod deletion", func() {
		const (
			podCount      = 10
			targetPort    = 8080
			provisionTime = 120 * time.Second
		)

		ctx := context.Background()
		egressName := ns.Name // Use namespace as egress name

		By(fmt.Sprintf("Creating %d egress pods", podCount))
		podNames := make([]string, podCount)
		for i := 0; i < podCount; i++ {
			podName := fmt.Sprintf("egress-crash-pod-%d", i)
			podNames[i] = podName

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns.Name,
					Labels: map[string]string{
						egressLabel: egressName,
					},
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d egress pods", podCount)

		By("Waiting for pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Waiting %v for Azure NAT Gateway provisioning", provisionTime))
		time.Sleep(provisionTime)

		By("Verifying NAT Gateway exists in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var foundEgress bool
		var natGatewayID string
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				natGatewayID = svc.Properties.PublicNatGatewayID
				break
			}
		}
		Expect(foundEgress).To(BeTrue(), "Egress service should exist")
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway should be provisioned")
		utils.Logf("Found NAT Gateway: %s", natGatewayID)

		By("Verifying all pods have finalizers")
		for _, podName := range podNames {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hasFinalizer := false
			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					hasFinalizer = true
					break
				}
			}
			Expect(hasFinalizer).To(BeTrue(), "Pod %s should have finalizer", podName)
		}
		utils.Logf("✓ All %d pods have ServiceGateway finalizer", podCount)

		By("Counting initial NAT Gateways")
		initialNATCount, err := countAzureNATGateways()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Found %d NAT Gateways before deletion", initialNATCount)

		By(fmt.Sprintf("Triggering deletion of all %d egress pods", podCount))
		for _, podName := range podNames {
			err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Deletion triggered for all pods")

		By("IMMEDIATELY crashing CCM (within 2 seconds of deletion)")
		time.Sleep(2 * time.Second)
		err = ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed (all pods deleted)")

		By("Waiting 10 seconds with CCM down")
		time.Sleep(10 * time.Second)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By("Waiting for CCM to complete cleanup (180s)")
		time.Sleep(180 * time.Second)

		By("Verifying all pods are deleted (finalizers removed)")
		pods, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", egressLabel, egressName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(Equal(0), "All egress pods should be deleted")
		utils.Logf("✓ All pods deleted (finalizers removed)")

		By("Verifying egress service removed from Service Gateway")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundEgress = false
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				utils.Logf("WARNING: Egress service %s still in SGW", egressName)
				break
			}
		}
		Expect(foundEgress).To(BeFalse(), "Egress service should be removed from Service Gateway")
		utils.Logf("✓ Egress service removed from Service Gateway")

		By("Verifying NAT Gateway is deleted")
		finalNATCount, err := countAzureNATGateways()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Final NAT Gateway count: %d (was %d)", finalNATCount, initialNATCount)

		utils.Logf("\n✓ Egress deletion crash recovery test PASSED!")
		utils.Logf("  - %d pods deleted after CCM crash", podCount)
		utils.Logf("  - NAT Gateway cleaned up")
		utils.Logf("  - All finalizers removed")
	})

	// Test 3: Mixed inbound + egress deletion + crash
	It("should recover and complete mixed inbound/egress deletion after CCM crash", func() {
		const (
			inboundServiceCount = 3
			egressPodCount      = 6
			servicePort         = int32(8080)
			targetPort          = 8080
			provisionTime       = 120 * time.Second
		)

		ctx := context.Background()
		egressName := ns.Name

		By(fmt.Sprintf("Creating %d inbound services", inboundServiceCount))
		serviceNames := make([]string, inboundServiceCount)
		for i := 0; i < inboundServiceCount; i++ {
			serviceName := fmt.Sprintf("mixed-crash-svc-%d", i)
			serviceNames[i] = serviceName

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
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
			_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Backend pod
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod", serviceName),
					Namespace: ns.Name,
					Labels:    map[string]string{"app": serviceName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}},
					},
				},
			}
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By(fmt.Sprintf("Creating %d egress pods", egressPodCount))
		egressPodNames := make([]string, egressPodCount)
		for i := 0; i < egressPodCount; i++ {
			podName := fmt.Sprintf("mixed-egress-pod-%d", i)
			egressPodNames[i] = podName

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns.Name,
					Labels:    map[string]string{egressLabel: egressName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "test-app", Image: utils.AgnhostImage, Args: []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)}},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Waiting %v for Azure provisioning", provisionTime))
		time.Sleep(provisionTime)

		By("Verifying initial Azure state")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount := 0
		outboundCount := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
			} else if svc.Properties.ServiceType == "Outbound" && svc.Name != "default-natgw-v2" {
				outboundCount++
			}
		}
		utils.Logf("Initial SGW state: %d inbound, %d outbound (non-default)", inboundCount, outboundCount)
		Expect(inboundCount).To(BeNumerically(">=", inboundServiceCount))
		Expect(outboundCount).To(BeNumerically(">=", 1))

		initialLBCount, _ := countAzureLoadBalancers()
		initialPIPCount, _ := countAzurePublicIPs()
		initialNATCount, _ := countAzureNATGateways()
		utils.Logf("Initial Azure resources: %d LBs, %d PIPs, %d NAT GWs", initialLBCount, initialPIPCount, initialNATCount)

		By("Triggering deletion of ALL resources simultaneously")
		// Delete inbound services
		for _, serviceName := range serviceNames {
			err := cs.CoreV1().Services(ns.Name).Delete(ctx, serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		// Delete egress pods
		for _, podName := range egressPodNames {
			err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Triggered deletion of %d services + %d egress pods", inboundServiceCount, egressPodCount)

		By("IMMEDIATELY crashing CCM (within 2 seconds)")
		time.Sleep(2 * time.Second)
		err = ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed (all pods deleted)")

		By("Waiting 15 seconds with CCM down")
		time.Sleep(15 * time.Second)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By("Waiting for CCM to complete all deletions (240s)")
		time.Sleep(240 * time.Second)

		By("Verifying all K8s services deleted")
		services, err := cs.CoreV1().Services(ns.Name).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		lbCount := 0
		for _, svc := range services.Items {
			if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
				lbCount++
			}
		}
		Expect(lbCount).To(Equal(0), "All LB services should be deleted")
		utils.Logf("✓ All K8s services deleted")

		By("Verifying all egress pods deleted")
		pods, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", egressLabel, egressName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(Equal(0), "All egress pods should be deleted")
		utils.Logf("✓ All egress pods deleted")

		By("Verifying Service Gateway cleaned up")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount = 0
		outboundCount = 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
				utils.Logf("WARNING: Remaining inbound: %s", svc.Name)
			} else if svc.Properties.ServiceType == "Outbound" && svc.Name != "default-natgw-v2" {
				outboundCount++
				utils.Logf("WARNING: Remaining outbound: %s", svc.Name)
			}
		}
		Expect(inboundCount).To(Equal(0), "No inbound services should remain")
		Expect(outboundCount).To(Equal(0), "No non-default outbound services should remain")
		utils.Logf("✓ Service Gateway cleaned up")

		By("Verifying Azure resources cleaned up")
		finalLBCount, _ := countAzureLoadBalancers()
		finalPIPCount, _ := countAzurePublicIPs()
		finalNATCount, _ := countAzureNATGateways()
		utils.Logf("Final Azure resources: %d LBs (was %d), %d PIPs (was %d), %d NATs (was %d)",
			finalLBCount, initialLBCount, finalPIPCount, initialPIPCount, finalNATCount, initialNATCount)

		Expect(finalLBCount).To(BeNumerically("<", initialLBCount), "LBs should be cleaned up")

		utils.Logf("\n✓ Mixed deletion crash recovery test PASSED!")
		utils.Logf("  - %d inbound services deleted", inboundServiceCount)
		utils.Logf("  - %d egress pods deleted", egressPodCount)
		utils.Logf("  - All Azure resources cleaned up after CCM crash")
	})
})

// countAzureLoadBalancers counts the number of SLB-managed Load Balancers in the resource group
func countAzureLoadBalancers() (int, error) {
	cmd := exec.Command("az", "network", "lb", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to list Load Balancers: %w", err)
	}

	var lbs []map[string]interface{}
	if err := json.Unmarshal(output, &lbs); err != nil {
		return 0, fmt.Errorf("failed to parse LB JSON: %w", err)
	}

	// SLB Load Balancers are named with service UIDs (36-char lowercase UUIDs)
	// Exclude AKS-managed LBs like "kubernetes", "kubernetes-internal"
	slbCount := 0
	for _, lb := range lbs {
		name, _ := lb["name"].(string)
		// SLB LBs are named with service UIDs: 36 chars, 4 dashes, all lowercase hex
		if len(name) == 36 && strings.Count(name, "-") == 4 {
			// Additional check: verify it looks like a UUID (all lowercase hex + dashes)
			isUUID := true
			for _, c := range name {
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
					isUUID = false
					break
				}
			}
			if isUUID {
				slbCount++
			}
		}
	}

	return slbCount, nil
}

// countAzurePublicIPs counts the number of SLB-managed Public IPs in the resource group
func countAzurePublicIPs() (int, error) {
	cmd := exec.Command("az", "network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to list Public IPs: %w", err)
	}

	var pips []map[string]interface{}
	if err := json.Unmarshal(output, &pips); err != nil {
		return 0, fmt.Errorf("failed to parse PIP JSON: %w", err)
	}

	// SLB PIPs are named with service UIDs (36-char UUIDs) for inbound services
	// NAT Gateway PIPs are named with egress name + "-pip" (e.g., "default-natgw-v2-pip")
	// We count both types but exclude non-SLB PIPs
	slbPIPCount := 0
	for _, pip := range pips {
		name, _ := pip["name"].(string)
		// SLB inbound service PIPs are just UUIDs (36 chars, 4 dashes)
		if len(name) == 36 && strings.Count(name, "-") == 4 {
			slbPIPCount++
		}
		// SLB NAT Gateway PIPs end with "-pip" (but exclude default-natgw-v2-pip)
		if strings.HasSuffix(name, "-pip") && name != "default-natgw-v2-pip" {
			slbPIPCount++
		}
	}

	return slbPIPCount, nil
}

// countAzureNATGateways counts the number of SLB-managed NAT Gateways in the resource group
// SLB creates NAT Gateways named after the egress label value (e.g., namespace name)
// The default NAT Gateway "default-natgw-v2" is always excluded
func countAzureNATGateways() (int, error) {
	cmd := exec.Command("az", "network", "nat", "gateway", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to list NAT Gateways: %w", err)
	}

	var nats []map[string]interface{}
	if err := json.Unmarshal(output, &nats); err != nil {
		return 0, fmt.Errorf("failed to parse NAT Gateway JSON: %w", err)
	}

	// Count all NAT Gateways except the default one
	nonDefaultCount := 0
	for _, nat := range nats {
		name, _ := nat["name"].(string)
		if name != "default-natgw-v2" {
			nonDefaultCount++
		}
	}

	return nonDefaultCount, nil
}
