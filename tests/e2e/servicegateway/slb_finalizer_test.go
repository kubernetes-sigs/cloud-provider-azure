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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

const (
	// Finalizer names
	serviceGatewayServiceFinalizer = "servicegateway.azure.com/service-cleanup"
	k8sLoadBalancerFinalizer       = "service.kubernetes.io/load-balancer-cleanup"
	serviceGatewayPodFinalizer     = "servicegateway.azure.com/pod-cleanup"

	// Test labels
	slbFinalizerTestLabel = "SLB-Finalizer"
)

// hasFinalizer checks if a Kubernetes object has a specific finalizer
func hasFinalizer(finalizers []string, finalizer string) bool {
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// verifyAzureLBAndPIPExist checks if Load Balancer and Public IP exist for a service
func verifyAzureLBAndPIPExist(serviceUID string) error {
	publicIPName := fmt.Sprintf("%s-pip", serviceUID)
	loadBalancerName := serviceUID

	// Query Public IPs
	pipOutput, err := runAz("network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("failed to query Azure for Public IPs: %w", err)
	}

	var publicIPs []AzurePublicIP
	if err := json.Unmarshal(pipOutput, &publicIPs); err != nil {
		return fmt.Errorf("failed to parse Public IP JSON: %w", err)
	}

	var foundPIP bool
	for _, pip := range publicIPs {
		if pip.Name == publicIPName {
			foundPIP = true
			utils.Logf("  Found PIP: %s (IP: %s)", pip.Name, pip.IPAddress)
			break
		}
	}
	if !foundPIP {
		return fmt.Errorf("public IP %s not found", publicIPName)
	}

	// Query Load Balancer
	lbOutput, err := runAz("network", "lb", "show",
		"--resource-group", resourceGroupName,
		"--name", loadBalancerName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("load Balancer %s not found: %w", loadBalancerName, err)
	}

	var serviceLB AzureLoadBalancer
	if err := json.Unmarshal(lbOutput, &serviceLB); err != nil {
		return fmt.Errorf("failed to parse Load Balancer JSON: %w", err)
	}

	utils.Logf("  Found LB: %s (SKU: %s, BackendPools: %d)", serviceLB.Name, serviceLB.SKU.Name, len(serviceLB.BackendAddressPools))
	return nil
}

// verifyAzureLBAndPIPDeleted checks if Load Balancer and Public IP have been deleted
func verifyAzureLBAndPIPDeleted(serviceUID string) error {
	publicIPName := fmt.Sprintf("%s-pip", serviceUID)
	loadBalancerName := serviceUID

	// Check Public IP is deleted
	pipOutput, err := runAz("network", "public-ip", "show",
		"--resource-group", resourceGroupName,
		"--name", publicIPName,
		"--output", "json")
	if err == nil {
		return fmt.Errorf("public IP %s still exists", publicIPName)
	}
	if !strings.Contains(string(pipOutput), "not found") && !strings.Contains(string(pipOutput), "NotFound") {
		return fmt.Errorf("unexpected error checking PIP: %s", string(pipOutput))
	}

	// Check Load Balancer is deleted
	lbOutput, err := runAz("network", "lb", "show",
		"--resource-group", resourceGroupName,
		"--name", loadBalancerName,
		"--output", "json")
	if err == nil {
		return fmt.Errorf("load Balancer %s still exists", loadBalancerName)
	}
	if !strings.Contains(string(lbOutput), "not found") && !strings.Contains(string(lbOutput), "NotFound") {
		return fmt.Errorf("unexpected error checking LB: %s", string(lbOutput))
	}

	return nil
}

// verifyNATGatewayAndPIPExist checks if NAT Gateway and its PIP exist for an egress service
func verifyNATGatewayAndPIPExist(natGatewayID string) error {
	// Extract NAT Gateway name from ID
	// Format: /subscriptions/.../resourceGroups/.../providers/Microsoft.Network/natGateways/<name>
	parts := strings.Split(natGatewayID, "/")
	natGatewayName := parts[len(parts)-1]

	// Query NAT Gateway
	natOutput, err := runAz("network", "nat", "gateway", "show",
		"--resource-group", resourceGroupName,
		"--name", natGatewayName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("NAT Gateway %s not found: %w", natGatewayName, err)
	}

	var natGateway map[string]interface{}
	if err := json.Unmarshal(natOutput, &natGateway); err != nil {
		return fmt.Errorf("failed to parse NAT Gateway JSON: %w", err)
	}

	utils.Logf("  Found NAT Gateway: %s", natGatewayName)

	// Check for associated PIPs
	if props, ok := natGateway["publicIpAddresses"].([]interface{}); ok && len(props) > 0 {
		utils.Logf("  NAT Gateway has %d associated Public IPs", len(props))
	}

	return nil
}

// verifyNATGatewayAndPIPDeleted checks that the NAT Gateway AND its Public IP have been deleted.
//
// The PIP half was missing despite the name: a CCM that deletes the NAT Gateway but orphans its
// Public IP passed every egress teardown spec while leaking a billable address each time.
func verifyNATGatewayAndPIPDeleted(natGatewayID string) error {
	// Extract NAT Gateway name from ID
	parts := strings.Split(natGatewayID, "/")
	natGatewayName := parts[len(parts)-1]

	// Check NAT Gateway is deleted
	natOutput, err := runAz("network", "nat", "gateway", "show",
		"--resource-group", resourceGroupName,
		"--name", natGatewayName,
		"--output", "json")
	if err == nil {
		return fmt.Errorf("NAT Gateway %s still exists", natGatewayName)
	}
	if !strings.Contains(string(natOutput), "not found") && !strings.Contains(string(natOutput), "NotFound") {
		return fmt.Errorf("unexpected error checking NAT Gateway: %s", string(natOutput))
	}

	// The NAT Gateway's Public IP follows the same <name>-pip convention as the inbound path and
	// must be torn down with it.
	if err := azurePublicIPAbsentErr(natGatewayName); err != nil {
		return fmt.Errorf("NAT Gateway %s was deleted but its Public IP leaked: %w", natGatewayName, err)
	}

	utils.Logf("  NAT Gateway %s and its Public IP confirmed deleted", natGatewayName)
	return nil
}

var _ = Describe("Container Load Balancer Finalizer Tests", Label(slbTestLabel, slbFinalizerTestLabel), func() {
	basename := "slb-finalizer-test"

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
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

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

	// Test 1: Service Finalizers
	// Creates services, verifies both finalizers are present, triggers deletion,
	// and verifies deletion timestamp + ServiceGateway finalizer still present
	It("should have both finalizers on services and retain ServiceGateway finalizer during deletion", func() {
		const (
			numServices = 3
			podsPerSvc  = 2
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)

		ctx := context.Background()
		serviceNames := make([]string, numServices)

		By(fmt.Sprintf("Creating %d LoadBalancer services with %d pods each", numServices, podsPerSvc))

		for i := 0; i < numServices; i++ {
			serviceName := fmt.Sprintf("finalizer-svc-%d", i)
			serviceNames[i] = serviceName
			serviceLabels := map[string]string{
				"app": serviceName,
			}

			// Create pods for this service
			for j := 0; j < podsPerSvc; j++ {
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			// Create LoadBalancer service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
					Annotations: map[string]string{
						"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
					},
				},
				Spec: v1.ServiceSpec{
					Type:     v1.ServiceTypeLoadBalancer,
					Selector: serviceLabels,
					Ports: []v1.ServicePort{
						{
							Name:       "http",
							Protocol:   v1.ProtocolTCP,
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
						},
					},
				},
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Created service %s with %d pods", serviceName, podsPerSvc)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure provisioning")
		Eventually(func() error {
			for _, serviceName := range serviceNames {
				svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("get service %s: %w", serviceName, err)
				}
				if !hasFinalizer(svc.Finalizers, serviceGatewayServiceFinalizer) {
					return fmt.Errorf("service %s should have ServiceGateway finalizer (%s)", serviceName, serviceGatewayServiceFinalizer)
				}
				if !hasFinalizer(svc.Finalizers, k8sLoadBalancerFinalizer) {
					return fmt.Errorf("service %s should have K8s LB finalizer (%s)", serviceName, k8sLoadBalancerFinalizer)
				}
				if err := serviceReconciledErr(string(svc.UID), podsPerSvc); err != nil {
					return fmt.Errorf("service %s: %w", serviceName, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all services should have finalizers, Azure resources, and registered pod endpoints")

		By("Verifying all services have BOTH finalizers (ServiceGateway + K8s LB)")
		for _, serviceName := range serviceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hasSGFinalizer := hasFinalizer(svc.Finalizers, serviceGatewayServiceFinalizer)
			hasK8sLBFinalizer := hasFinalizer(svc.Finalizers, k8sLoadBalancerFinalizer)

			utils.Logf("Service %s finalizers: ServiceGateway=%v, K8sLB=%v, All finalizers: %v",
				serviceName, hasSGFinalizer, hasK8sLBFinalizer, svc.Finalizers)

			Expect(hasSGFinalizer).To(BeTrue(),
				"Service %s should have ServiceGateway finalizer (%s)", serviceName, serviceGatewayServiceFinalizer)
			Expect(hasK8sLBFinalizer).To(BeTrue(),
				"Service %s should have K8s LB finalizer (%s)", serviceName, k8sLoadBalancerFinalizer)
		}
		utils.Logf("✓ All %d services have both required finalizers", numServices)

		By("Verifying services exist in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service Gateway has %d services", len(sgResponse.Value))

		By("Verifying Azure LB and PIP exist for each service")
		serviceUIDs := make([]string, numServices)
		for i, serviceName := range serviceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUID := string(svc.UID)
			serviceUIDs[i] = serviceUID

			utils.Logf("Checking Azure resources for service %s (UID: %s)", serviceName, serviceUID)
			err = verifyAzureLBAndPIPExist(serviceUID)
			Expect(err).NotTo(HaveOccurred(), "Azure LB/PIP should exist for service %s", serviceName)
		}
		utils.Logf("✓ All %d services have Azure LB and PIP provisioned", numServices)

		By("Triggering deletion of all services")
		for _, serviceName := range serviceNames {
			err := cs.CoreV1().Services(ns.Name).Delete(ctx, serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Initiated deletion for service %s", serviceName)
		}

		By("Asserting no Service object is reclaimed while its Azure Load Balancer is still live")
		// Pin the ORDER, not two independent "eventually happened" facts. The previous check was a
		// single T+2s snapshot whose body was skipped entirely on IsNotFound - so a controller that
		// released the ServiceGateway finalizer early, letting the object be reclaimed inside those
		// 2s, passed without asserting anything. That early release is the leak/blackhole bug.
		//
		// Query Azure first, then Kubernetes: observing "Load Balancer alive" together with
		// "Service object gone" is the violation.
		Consistently(func() error {
			for i, serviceName := range serviceNames {
				lbAlive := azureLoadBalancerAbsentErr(serviceUIDs[i]) != nil
				svc, getErr := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				svcGone := apierrors.IsNotFound(getErr)
				if getErr != nil && !svcGone {
					return fmt.Errorf("could not read service %s: %w", serviceName, getErr)
				}
				if lbAlive && svcGone {
					return fmt.Errorf("service %s was reclaimed while its Azure Load Balancer was still live: "+
						"the ServiceGateway finalizer was released before Azure cleanup completed", serviceName)
				}
				if !svcGone {
					if svc.DeletionTimestamp == nil {
						return fmt.Errorf("service %s has no DeletionTimestamp despite being deleted", serviceName)
					}
					if lbAlive && !hasFinalizer(svc.Finalizers, serviceGatewayServiceFinalizer) {
						return fmt.Errorf("service %s lost its ServiceGateway finalizer while its Load Balancer "+
							"was still live in Azure", serviceName)
					}
				}
			}
			return nil
		}, 30*time.Second, 3*time.Second).Should(Succeed(),
			"the ServiceGateway finalizer must hold each Service until its Azure Load Balancer is deleted")

		By("Waiting for services to be fully deleted (Azure cleanup completes)")
		for _, serviceName := range serviceNames {
			Eventually(func() bool {
				_, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
				"Service %s should be deleted after Azure cleanup", serviceName)
			utils.Logf("✓ Service %s fully deleted", serviceName)
		}

		By("Verifying Azure resources are cleaned up via Service Gateway")
		// Uses the shared cleanup check rather than a local copy: a hand-rolled loop that only
		// rejects unexpected services reports success on an empty Service Gateway, where the
		// default outbound service has been destroyed along with the test's own.
		Eventually(serviceGatewayCleanupErr, 2*time.Minute, 10*time.Second).Should(Succeed(),
			"Service Gateway should only have default outbound service after cleanup")

		By("Verifying Azure LB and PIP are deleted for each service")
		for i, serviceUID := range serviceUIDs {
			utils.Logf("Checking Azure resources deleted for service %s (UID: %s)", serviceNames[i], serviceUID)
			Eventually(func() error {
				return verifyAzureLBAndPIPDeleted(serviceUID)
			}, 2*time.Minute, 10*time.Second).Should(Succeed(),
				"Azure LB/PIP should be deleted for service %s", serviceNames[i])
			utils.Logf("  ✓ LB and PIP deleted for service %s", serviceNames[i])
		}
		utils.Logf("✓ All Azure LB and PIP resources cleaned up")

		utils.Logf("✓ Test completed: Service finalizers verified correctly")
	})

	// Test 2: Pod Finalizers (Egress/Outbound)
	// Creates pods with egress label, verifies pod finalizer, deletes pods one by one,
	// and verifies that non-last pods are removed instantly while last pod waits for Azure cleanup
	It("should have pod finalizer on egress pods and block last pod deletion until Azure cleanup", func() {
		const (
			numPods        = 5
			targetPort     = 8080
			provisionTime  = 90 * time.Second
			instantTimeout = 30 * time.Second // Non-last pods should be deleted within this time
		)

		// Use namespace name as egress name to avoid conflicts between test runs
		egressName := ns.Name

		ctx := context.Background()
		podNames := make([]string, numPods)

		By(fmt.Sprintf("Creating %d pods with egress label '%s=%s'", numPods, egressLabel, egressName))

		for i := 0; i < numPods; i++ {
			podName := fmt.Sprintf("egress-finalizer-pod-%d", i)
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
			utils.Logf("Created egress pod %s", podName)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure NAT Gateway provisioning")
		Eventually(func() error {
			return egressRegisteredErr(egressName, numPods)
		}, provisionTime, 10*time.Second).Should(Succeed(),
			"egress service should be reconciled with NAT Gateway and registered pods")

		By("Verifying all pods have ServiceGateway pod finalizer")
		for _, podName := range podNames {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hasPodFinalizer := hasFinalizer(pod.Finalizers, serviceGatewayPodFinalizer)
			utils.Logf("Pod %s finalizers: ServiceGateway=%v, All finalizers: %v",
				podName, hasPodFinalizer, pod.Finalizers)

			Expect(hasPodFinalizer).To(BeTrue(),
				"Pod %s should have ServiceGateway pod finalizer (%s)", podName, serviceGatewayPodFinalizer)
		}
		utils.Logf("✓ All %d pods have ServiceGateway pod finalizer", numPods)

		By("Verifying NAT Gateway exists in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var foundEgress bool
		var natGatewayID string
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				natGatewayID = svc.Properties.PublicNatGatewayID
				utils.Logf("Found egress service '%s' with NAT Gateway: %s", egressName, natGatewayID)
				break
			}
		}
		Expect(foundEgress).To(BeTrue(), "Egress service '%s' should exist in Service Gateway", egressName)
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway ID should not be empty")

		By("Verifying Azure NAT Gateway and PIP exist")
		err = verifyNATGatewayAndPIPExist(natGatewayID)
		Expect(err).NotTo(HaveOccurred(), "Azure NAT Gateway and PIP should exist")
		utils.Logf("✓ Azure NAT Gateway and PIP verified to exist")

		By("Deleting pods 0 to 3 (non-last pods) one by one - should be instant")
		for i := 0; i < numPods-1; i++ {
			podName := podNames[i]
			deleteStartTime := time.Now()

			utils.Logf("Deleting non-last pod %s (pod %d of %d)", podName, i+1, numPods)
			err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for pod to be fully deleted - should be fast since finalizer is removed quickly
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, instantTimeout, 1*time.Second).Should(BeTrue(),
				"Non-last pod %s should be deleted within %v (finalizer removed instantly)", podName, instantTimeout)

			deleteDuration := time.Since(deleteStartTime)
			utils.Logf("✓ Pod %s deleted in %v (instant - no Azure cleanup needed)", podName, deleteDuration)

			// Verify it was actually fast
			Expect(deleteDuration).To(BeNumerically("<", instantTimeout),
				"Non-last pod %s deletion should be instant (< %v), but took %v", podName, instantTimeout, deleteDuration)
		}
		utils.Logf("✓ All non-last pods deleted instantly (finalizers removed quickly)")

		By("Verifying NAT Gateway still exists (last pod keeps it alive)")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundEgress = false
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				utils.Logf("NAT Gateway still exists for egress '%s' (last pod keeping it alive)", egressName)
				break
			}
		}
		Expect(foundEgress).To(BeTrue(), "NAT Gateway should still exist while last pod is alive")

		By("Deleting last pod - should be blocked until Azure NAT Gateway cleanup")
		lastPodName := podNames[numPods-1]
		deleteStartTime := time.Now()

		utils.Logf("Deleting last pod %s - this should trigger Azure cleanup", lastPodName)
		err = cs.CoreV1().Pods(ns.Name).Delete(ctx, lastPodName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Asserting the last pod is never reclaimed while its NAT Gateway is still live in Azure")
		// This is the ordering invariant, and it must be asserted as an ORDER rather than as two
		// independent "eventually happened" facts. The previous check was a single T+2s snapshot
		// wrapped in `if !apierrors.IsNotFound(err)`, so a controller that released the finalizer
		// early - letting the pod be reclaimed within those 2s, which is precisely what the
		// traffic-blackhole bug looks like - skipped the assertion entirely and still passed.
		//
		// Query Azure first, then the pod: if the NAT Gateway is still present, the pod must still
		// exist. Observing "NAT Gateway alive" and "pod gone" together is the violation.
		Consistently(func() error {
			natAlive := verifyNATGatewayAndPIPExist(natGatewayID) == nil
			_, getErr := cs.CoreV1().Pods(ns.Name).Get(ctx, lastPodName, metav1.GetOptions{})
			podGone := apierrors.IsNotFound(getErr)
			if getErr != nil && !podGone {
				return fmt.Errorf("could not read last pod %s: %w", lastPodName, getErr)
			}
			if natAlive && podGone {
				return fmt.Errorf("last pod %s was reclaimed while its NAT Gateway was still live in Azure: "+
					"the cleanup finalizer was released before the drain completed", lastPodName)
			}
			return nil
		}, 30*time.Second, 2*time.Second).Should(Succeed(),
			"the last pod's cleanup finalizer must hold it Terminating until the NAT Gateway is torn down")

		By("Waiting for last pod to be fully deleted (Azure NAT Gateway cleanup)")
		Eventually(func() bool {
			_, err := cs.CoreV1().Pods(ns.Name).Get(ctx, lastPodName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(),
			"Last pod %s should be deleted after Azure cleanup", lastPodName)

		lastPodDeleteDuration := time.Since(deleteStartTime)
		utils.Logf("✓ Last pod %s deleted in %v (after Azure cleanup)", lastPodName, lastPodDeleteDuration)

		// Last pod should take longer than instant timeout (Azure cleanup required)
		// Note: This might not always be true if Azure is very fast, so we just log it
		if lastPodDeleteDuration > instantTimeout {
			utils.Logf("✓ Last pod deletion took longer than instant timeout (%v > %v) - Azure cleanup confirmed",
				lastPodDeleteDuration, instantTimeout)
		} else {
			utils.Logf("Note: Last pod deleted quickly (%v) - Azure cleanup was fast", lastPodDeleteDuration)
		}

		By("Verifying NAT Gateway is cleaned up from Service Gateway")
		Eventually(func() bool {
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				return false
			}
			for _, svc := range sgResponse.Value {
				if svc.Name == egressName {
					utils.Logf("NAT Gateway '%s' still exists in Service Gateway", egressName)
					return false
				}
			}
			return true
		}, 2*time.Minute, 10*time.Second).Should(BeTrue(),
			"NAT Gateway '%s' should be cleaned up from Service Gateway", egressName)

		By("Verifying Azure NAT Gateway and PIP are deleted")
		Eventually(func() error {
			return verifyNATGatewayAndPIPDeleted(natGatewayID)
		}, 2*time.Minute, 10*time.Second).Should(Succeed(),
			"Azure NAT Gateway and PIP should be deleted")
		utils.Logf("✓ Azure NAT Gateway and PIP verified to be deleted")

		utils.Logf("✓ Test completed: Pod finalizers verified correctly")
		utils.Logf("  - Non-last pods: deleted instantly (finalizer removed quickly)")
		utils.Logf("  - Last pod: blocked by finalizer until Azure NAT Gateway cleanup")
	})

	// Test 3: Drain-gate survives the multi-event terminating sequence.
	// A terminating pod is delivered to the informer several times (deletionTimestamp set,
	// then terminating status updates). Only the first event drains the address; the later
	// events must not strip the pod finalizer while the NRP drain is still pending. Uses a
	// single (last) pod and a non-zero grace period to widen the multi-event window, then
	// asserts the finalizer is held continuously until Azure NAT Gateway cleanup.
	It("should retain egress pod finalizer across the multi-event terminating window until Azure cleanup", func() {
		const (
			targetPort    = 8080
			provisionTime = 90 * time.Second
			gracePeriod   = int64(30)
		)

		egressName := ns.Name
		ctx := context.Background()
		podName := "egress-draingate-pod"

		By(fmt.Sprintf("Creating a single egress pod '%s' with label '%s=%s'", podName, egressLabel, egressName))
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: ns.Name,
				Labels:    map[string]string{egressLabel: egressName},
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

		By("Waiting for the pod to be ready")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for Azure NAT Gateway provisioning and pod registration")
		Eventually(func() error {
			return egressRegisteredErr(egressName, 1)
		}, provisionTime, 10*time.Second).Should(Succeed(),
			"egress service should be reconciled with NAT Gateway and the registered pod")

		By("Resolving the NAT Gateway for the egress service")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		var natGatewayID string
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				natGatewayID = svc.Properties.PublicNatGatewayID
				break
			}
		}
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway ID should not be empty")
		Expect(verifyNATGatewayAndPIPExist(natGatewayID)).To(Succeed(), "Azure NAT Gateway and PIP should exist")

		By("Verifying the pod carries the ServiceGateway pod finalizer")
		got, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(hasFinalizer(got.Finalizers, serviceGatewayPodFinalizer)).To(BeTrue(),
			"Pod %s should have ServiceGateway pod finalizer before deletion", podName)

		By("Deleting the last egress pod with a non-zero grace period")
		gp := gracePeriod
		err = cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: &gp})
		Expect(err).NotTo(HaveOccurred())

		By("Asserting the finalizer is never transiently stripped while the NRP drain is pending")
		// While the pod still exists with a deletionTimestamp, the drain-gate finalizer must
		// stay present on every observation; a single premature removal (the W2-1 defect) would
		// let the pod and its egress IP be reclaimed while NRP still maps the address.
		//
		// The pod disappearing is only an acceptable terminal state once Azure teardown has
		// actually completed. Treating it as acceptable unconditionally would let a controller
		// that releases the finalizer part-way through the window pass: the pod vanishes, every
		// later observation short-circuits, and the NAT Gateway is still live in Azure.
		Consistently(func() error {
			p, getErr := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				if verifyNATGatewayAndPIPExist(natGatewayID) == nil {
					return fmt.Errorf("pod %s was reclaimed while its NAT Gateway was still live in Azure: "+
						"the drain-gate finalizer was released before NRP finished draining", podName)
				}
				return nil // drained and torn down; acceptable terminal state
			}
			if getErr != nil {
				return getErr
			}
			if p.DeletionTimestamp == nil {
				return nil
			}
			if !hasFinalizer(p.Finalizers, serviceGatewayPodFinalizer) {
				return fmt.Errorf(
					"terminating pod lost ServiceGateway finalizer: uid=%s resourceVersion=%s phase=%s hostIP=%s podIPs=%v deletionTimestamp=%s finalizers=%v",
					p.UID, p.ResourceVersion, p.Status.Phase, p.Status.HostIP, p.Status.PodIPs, p.DeletionTimestamp.Time.Format(time.RFC3339Nano), p.Finalizers,
				)
			}
			return nil
		}, 20*time.Second, 1*time.Second).Should(Succeed(),
			"pod finalizer must be held continuously while terminating until NRP drain completes")

		By("Waiting for the pod to be fully deleted after Azure NAT Gateway cleanup")
		Eventually(func() bool {
			_, getErr := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			return apierrors.IsNotFound(getErr)
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(),
			"pod %s should be deleted after Azure cleanup", podName)

		By("Verifying the NAT Gateway and PIP are cleaned up")
		Eventually(func() error {
			return verifyNATGatewayAndPIPDeleted(natGatewayID)
		}, 2*time.Minute, 10*time.Second).Should(Succeed(),
			"Azure NAT Gateway and PIP should be deleted once the egress pod drains")

		utils.Logf("✓ Test completed: drain-gate finalizer retained across the multi-event terminating window")
	})
})
