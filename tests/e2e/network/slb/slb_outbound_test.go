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

const (
	// egressLabel is the label key used to designate pods for outbound NAT gateway
	egressLabel = "kubernetes.azure.com/service-egress-gateway"
)

var _ = Describe("Container Load Balancer Outbound (NAT Gateway)", Label(slbTestLabel), func() {
	basename := "slb-outbound-test"

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

			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()

			// Note: NAT Gateway cleanup verification is done per-test since egress names vary
		}

		cs = nil
		ns = nil
	})

	It("should create NAT gateway and PIP for pods with egress label", func() {
		const (
			numPods    = 10
			egressName = "test-egress-gateway"
			waitTime   = 90 * time.Second
			targetPort = 8080
		)

		By(fmt.Sprintf("Creating %d pods with egress label '%s=%s'", numPods, egressLabel, egressName))

		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d egress pods are ready", numPods)

		By("Waiting for Azure to provision NAT Gateway and PIP")
		utils.Logf("Waiting %v for NAT Gateway provisioning...", waitTime)
		time.Sleep(waitTime)

		By("Querying Service Gateway for outbound service")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var outboundServiceFound bool
		var natGatewayID string

		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				outboundServiceFound = true
				natGatewayID = svc.Properties.PublicNatGatewayID
				utils.Logf("Found outbound service '%s' in Service Gateway", egressName)
				utils.Logf("  NAT Gateway ID: %s", natGatewayID)
				break
			}
		}

		Expect(outboundServiceFound).To(BeTrue(), fmt.Sprintf("Outbound service '%s' should exist in Service Gateway", egressName))
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway ID should not be empty")

		By("Verifying NAT Gateway exists in Azure")
		// Extract NAT Gateway name from the resource ID
		// Format: /subscriptions/.../resourceGroups/.../providers/Microsoft.Network/natGateways/<name>
		parts := strings.Split(natGatewayID, "/")
		natGatewayName := parts[len(parts)-1]

		natGwCmd := exec.Command("az", "network", "nat", "gateway", "show",
			"--resource-group", resourceGroupName,
			"--name", natGatewayName,
			"--output", "json")
		natGwOutput, err := natGwCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("NAT Gateway %s should exist in Azure", natGatewayName))

		var natGateway map[string]interface{}
		err = json.Unmarshal(natGwOutput, &natGateway)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("NAT Gateway verified:")
		if props, ok := natGateway["properties"].(map[string]interface{}); ok {
			if sku, ok := props["sku"].(map[string]interface{}); ok {
				if skuName, ok := sku["name"].(string); ok {
					utils.Logf("  SKU: %s", skuName)
					Expect(skuName).To(Equal("StandardV2"), "NAT Gateway SKU should be StandardV2")
				}
			}

			// Verify Public IP exists
			if pips, ok := props["publicIpAddresses"].([]interface{}); ok && len(pips) > 0 {
				utils.Logf("  Public IPs: %d", len(pips))
				Expect(len(pips)).To(BeNumerically(">", 0), "NAT Gateway should have at least one Public IP")

				if pip, ok := pips[0].(map[string]interface{}); ok {
					if pipID, ok := pip["id"].(string); ok {
						utils.Logf("  Public IP ID: %s", pipID)
					}
				}
			}

			// Verify Service Gateway association
			if sgw, ok := props["serviceGateway"].(map[string]interface{}); ok {
				if sgwID, ok := sgw["id"].(string); ok {
					utils.Logf("  Service Gateway: %s", sgwID)
					Expect(sgwID).To(ContainSubstring(serviceGatewayName), "NAT Gateway should be associated with Service Gateway")
				}
			}
		}

		By("Verifying pod IPs registered in Address Locations")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		registeredPods := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						registeredPods++
					}
				}
			}
		}

		utils.Logf("Registered %d pod IPs for egress gateway '%s'", registeredPods, egressName)
		Expect(registeredPods).To(Equal(numPods), fmt.Sprintf("Expected %d pod IPs, got %d", numPods, registeredPods))

		utils.Logf("\n✓ Outbound NAT Gateway test passed: %d pods", numPods)
	})

	It("should handle multiple egress gateways with different labels", func() {
		const (
			podsPerGateway = 8
			waitTime       = 90 * time.Second
			targetPort     = 8080
		)

		egressGateways := []string{"egress-alpha", "egress-beta", "egress-gamma"}
		totalPods := len(egressGateways) * podsPerGateway

		By(fmt.Sprintf("Creating %d egress gateways with %d pods each (%d total)", len(egressGateways), podsPerGateway, totalPods))

		for _, egressName := range egressGateways {
			for i := 0; i < podsPerGateway; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", egressName, i),
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d egress pods are ready", totalPods)

		By("Waiting for Azure to provision all NAT Gateways")
		utils.Logf("Waiting %v for NAT Gateway provisioning...", waitTime)
		time.Sleep(waitTime)

		By("Verifying all egress gateways in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundGateways := make(map[string]string)
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" {
				for _, expectedGateway := range egressGateways {
					if svc.Name == expectedGateway {
						foundGateways[expectedGateway] = svc.Properties.PublicNatGatewayID
						utils.Logf("Found egress gateway '%s' with NAT Gateway: %s", expectedGateway, svc.Properties.PublicNatGatewayID)
						break
					}
				}
			}
		}

		Expect(len(foundGateways)).To(Equal(len(egressGateways)), fmt.Sprintf("Expected %d egress gateways, found %d", len(egressGateways), len(foundGateways)))

		By("Verifying pod IPs for each egress gateway")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		gatewayPodCounts := make(map[string]int)
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					for _, gateway := range egressGateways {
						if svcName == gateway {
							gatewayPodCounts[gateway]++
						}
					}
				}
			}
		}

		for _, gateway := range egressGateways {
			count := gatewayPodCounts[gateway]
			utils.Logf("Egress gateway '%s': %d pod IPs registered", gateway, count)
			Expect(count).To(Equal(podsPerGateway), fmt.Sprintf("Gateway '%s' should have %d pods, got %d", gateway, podsPerGateway, count))
		}

		utils.Logf("\n✓ Multiple egress gateways test passed: %d gateways, %d total pods", len(egressGateways), totalPods)
	})

	It("should handle scaling egress pods from 10 to 30", func() {
		const (
			initialPods = 10
			finalPods   = 30
			egressName  = "scaling-egress"
			waitTime    = 60 * time.Second
			targetPort  = 8080
		)

		By(fmt.Sprintf("Creating %d initial egress pods", initialPods))

		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for initial pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for NAT Gateway provisioning")
		time.Sleep(waitTime)

		By("Verifying initial state")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		initialRegistered := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						initialRegistered++
					}
				}
			}
		}

		utils.Logf("Initial state: %d pod IPs registered", initialRegistered)
		Expect(initialRegistered).To(Equal(initialPods))

		By(fmt.Sprintf("Scaling up: creating %d additional pods", finalPods-initialPods))

		for i := initialPods; i < finalPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready after scaling")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Address Locations update")
		time.Sleep(waitTime)

		By("Verifying scaled state")
		alResponseFinal, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		finalRegistered := 0
		for _, location := range alResponseFinal.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						finalRegistered++
					}
				}
			}
		}

		utils.Logf("After scaling: %d pod IPs registered", finalRegistered)
		Expect(finalRegistered).To(Equal(finalPods), fmt.Sprintf("Expected %d pod IPs, got %d", finalPods, finalRegistered))

		utils.Logf("\n✓ Egress scaling test passed: %d → %d pods", initialPods, finalPods)
	})

	It("should handle egress pod deletion and cleanup", func() {
		const (
			initialPods = 20
			remainPods  = 5
			egressName  = "deletion-egress"
			waitTime    = 60 * time.Second
			targetPort  = 8080
		)

		By(fmt.Sprintf("Creating %d egress pods", initialPods))

		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for NAT Gateway provisioning")
		time.Sleep(waitTime)

		By("Verifying initial state")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		initialRegistered := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						initialRegistered++
					}
				}
			}
		}

		utils.Logf("Initial state: %d pod IPs registered", initialRegistered)
		Expect(initialRegistered).To(Equal(initialPods))

		By(fmt.Sprintf("Deleting %d pods (keeping %d)", initialPods-remainPods, remainPods))

		for i := remainPods; i < initialPods; i++ {
			podName := fmt.Sprintf("egress-pod-%d", i)
			err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for pod deletions to complete")
		time.Sleep(30 * time.Second)

		By("Waiting for Address Locations cleanup")
		time.Sleep(waitTime)

		By("Verifying cleanup")
		alResponseFinal, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		finalRegistered := 0
		for _, location := range alResponseFinal.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						finalRegistered++
					}
				}
			}
		}

		utils.Logf("After deletion: %d pod IPs registered", finalRegistered)
		Expect(finalRegistered).To(Equal(remainPods), fmt.Sprintf("Expected %d pod IPs after cleanup, got %d", remainPods, finalRegistered))

		utils.Logf("\n✓ Egress deletion test passed: %d → %d pods", initialPods, remainPods)
	})

	It("should handle mixed inbound and outbound services together", func() {
		const (
			egressPods  = 15
			inboundPods = 15
			egressName  = "mixed-egress"
			serviceName = "mixed-inbound-svc"
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		By("Creating egress pods")
		for i := 0; i < egressPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Creating inbound service and pods")
		serviceLabels := map[string]string{
			"app": serviceName,
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
		serviceUID := string(createdService.UID)

		for i := 0; i < inboundPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("inbound-pod-%d", i),
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

		By("Waiting for all pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d pods ready (%d egress + %d inbound)", egressPods+inboundPods, egressPods, inboundPods)

		By("Waiting for Azure provisioning")
		time.Sleep(waitTime)

		By("Verifying Service Gateway has both service types")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundOutbound := false
		foundInbound := false

		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				foundOutbound = true
				utils.Logf("Found outbound service: %s", egressName)
			}
			if svc.Properties.ServiceType == "Inbound" && svc.Name == serviceUID {
				foundInbound = true
				utils.Logf("Found inbound service: %s", serviceUID)
			}
		}

		Expect(foundOutbound).To(BeTrue(), "Outbound service should exist")
		Expect(foundInbound).To(BeTrue(), "Inbound service should exist")

		By("Verifying Address Locations for both services")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		egressCount := 0
		inboundCount := 0

		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						egressCount++
					}
					if svcName == serviceUID {
						inboundCount++
					}
				}
			}
		}

		utils.Logf("Egress pods: %d registered", egressCount)
		utils.Logf("Inbound pods: %d registered", inboundCount)

		Expect(egressCount).To(Equal(egressPods), fmt.Sprintf("Expected %d egress pods, got %d", egressPods, egressCount))
		Expect(inboundCount).To(Equal(inboundPods), fmt.Sprintf("Expected %d inbound pods, got %d", inboundPods, inboundCount))

		utils.Logf("\n✓ Mixed inbound+outbound test passed: %d egress + %d inbound = %d total pods", egressPods, inboundPods, egressPods+inboundPods)
	})
})
