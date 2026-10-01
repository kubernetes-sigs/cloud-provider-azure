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
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
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
		// Set by a spec that scales the CCM down. AfterEach restores it before the cleanup that needs
		// a running CCM (DeferCleanup would run only after AfterEach).
		ccmToRestore *CCMClusterClient
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if ccmToRestore != nil {
			_ = scaleCCMDeployment(context.TODO(), ccmToRestore, 1)
			_ = waitForCCMFullyUp(context.TODO(), ccmToRestore, CCMRecoveryTimeout)
			ccmToRestore = nil
		}
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Azure cleanup to complete (egress gateway cleanup is slower)")
			eventuallyAzureCleanup(6 * time.Minute)

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
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != numPods {
				return fmt.Errorf("expected %d live egress pod IPs, got %d", numPods, len(want))
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be registered with NAT Gateway and pod IPs")

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

		// Fetch with `az rest` at the ServiceGateway API version. `az network nat gateway show`
		// deserializes an NRP-managed NAT gateway's properties as EMPTY, so every assertion below
		// used to sit behind a type-assertion that silently failed: the live ss10 run logged
		// "NAT Gateway verified:" but never once logged the SKU, proving the whole block was
		// skipped and this step asserted nothing beyond the CLI call succeeding.
		natGwOutput, err := runAz("rest", "--method", "get",
			"--url", fmt.Sprintf("https://management.azure.com%s?api-version=%s", natGatewayID, apiVersion))
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("NAT Gateway %s should exist in Azure", natGatewayName))

		// `sku` is a top-level field on the ARM NAT Gateway resource, a sibling of `properties` -
		// not nested inside it. Decoding it under `properties` yields an empty string, which is
		// what the first live run of this (previously skipped) assertion reported.
		var natGateway struct {
			Tags map[string]string `json:"tags"`
			SKU  struct {
				Name string `json:"name"`
			} `json:"sku"`
			Properties struct {
				PublicIPAddresses []struct {
					ID string `json:"id"`
				} `json:"publicIpAddresses"`
				ServiceGateway struct {
					ID string `json:"id"`
				} `json:"serviceGateway"`
			} `json:"properties"`
		}
		Expect(json.Unmarshal(natGwOutput, &natGateway)).To(Succeed())

		utils.Logf("NAT Gateway verified:")
		props := natGateway.Properties

		utils.Logf("  SKU: %s", natGateway.SKU.Name)
		Expect(natGateway.SKU.Name).To(Equal("StandardV2"), "NAT Gateway SKU should be StandardV2")

		utils.Logf("  Public IPs: %d", len(props.PublicIPAddresses))
		Expect(props.PublicIPAddresses).NotTo(BeEmpty(), "NAT Gateway should have at least one Public IP")
		utils.Logf("  Public IP ID: %s", props.PublicIPAddresses[0].ID)

		Expect(natGateway.Tags).To(HaveKeyWithValue("k8s-azure-egress-identity", egressName),
			"a managed NAT Gateway must carry its egress identity tag, which orphan cleanup relies on")

		utils.Logf("  Service Gateway: %s", props.ServiceGateway.ID)
		Expect(props.ServiceGateway.ID).To(ContainSubstring(serviceGatewayName),
			"NAT Gateway should be associated with the Service Gateway")

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
		Eventually(func() error {
			for _, egressName := range egressGateways {
				want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
				if err != nil {
					return err
				}
				if len(want) != podsPerGateway {
					return fmt.Errorf("egress %s: expected %d live pod IPs, got %d", egressName, podsPerGateway, len(want))
				}
				if err := egressRegisteredMatchErr(egressName, want); err != nil {
					return err
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all egress gateways should be registered with NAT Gateways and pod IPs")

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
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != initialPods {
				return fmt.Errorf("expected %d live egress pod IPs, got %d", initialPods, len(want))
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be registered with initial pod IPs")

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
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != finalPods {
				return fmt.Errorf("expected %d live egress pod IPs, got %d", finalPods, len(want))
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be updated with scaled pod IPs")

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
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != initialPods {
				return fmt.Errorf("expected %d live egress pod IPs, got %d", initialPods, len(want))
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be registered with initial pod IPs")

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

		By("Waiting for the deleted pods' addresses to drain, leaving exactly the survivors")
		// Assert the exact surviving set. A count cannot tell a correct drain from an inverted one:
		// draining the survivors and leaving the deleted pods registered yields the same number
		// while blackholing every live pod's egress traffic.
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != remainPods {
				return fmt.Errorf("expected %d surviving egress pod IPs, got %d", remainPods, len(want))
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"exactly the surviving egress pods' IPs must remain registered after deletion")

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
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != egressPods {
				return fmt.Errorf("egress %s: expected %d live pod IPs, got %d", egressName, egressPods, len(want))
			}
			if err := egressRegisteredMatchErr(egressName, want); err != nil {
				return err
			}
			if err := serviceReconciledErr(serviceUID, inboundPods); err != nil {
				return err
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"inbound service and egress service should be registered with pod IPs")

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

	It("should handle pods with both inbound LB and outbound NAT Gateway", func() {
		const (
			dualPods    = 10
			egressName  = "dual-egress"
			serviceName = "dual-inbound-svc"
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		By("Creating LoadBalancer Service for inbound traffic")
		serviceLabels := map[string]string{
			"app": "dual-traffic-pod",
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
		utils.Logf("Created LoadBalancer service: %s (UID: %s)", serviceName, serviceUID)

		By("Creating pods with BOTH inbound (LB selector) AND outbound (egress label)")
		for i := 0; i < dualPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("dual-pod-%d", i),
					Namespace: ns.Name,
					Labels: map[string]string{
						// Label for LB Service selector (inbound)
						"app": "dual-traffic-pod",
						// Label for egress NAT Gateway (outbound)
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
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d dual-traffic pods ready", dualPods)

		By("Waiting for Azure provisioning")
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) != dualPods {
				return fmt.Errorf("egress %s: expected %d live pod IPs, got %d", egressName, dualPods, len(want))
			}
			if err := egressRegisteredMatchErr(egressName, want); err != nil {
				return err
			}
			if err := serviceReconciledErr(serviceUID, dualPods); err != nil {
				return err
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"inbound service and egress service should both include all dual-traffic pods")

		By("Verifying Service Gateway has both inbound and outbound services")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundOutbound := false
		foundInbound := false

		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				foundOutbound = true
				utils.Logf("Found outbound service (NAT Gateway): %s", egressName)
			}
			if svc.Properties.ServiceType == "Inbound" && svc.Name == serviceUID {
				foundInbound = true
				utils.Logf("Found inbound service (LB): %s", serviceUID)
			}
		}

		Expect(foundOutbound).To(BeTrue(), "Outbound NAT Gateway service should exist")
		Expect(foundInbound).To(BeTrue(), "Inbound LB service should exist")

		By("Verifying each pod is registered in BOTH inbound and outbound services")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		// Track which pod IPs are in each service
		egressPodIPs := make(map[string]bool)
		inboundPodIPs := make(map[string]bool)

		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						egressPodIPs[addr.Address] = true
					}
					if svcName == serviceUID {
						inboundPodIPs[addr.Address] = true
					}
				}
			}
		}

		utils.Logf("Pod IPs in outbound (NAT Gateway): %d", len(egressPodIPs))
		utils.Logf("Pod IPs in inbound (LB): %d", len(inboundPodIPs))

		// Counting registrations cannot distinguish a correct registration from a stale or foreign
		// address: two wrong pod IPs satisfy a count of two just as well as the two right ones.
		// The dual-traffic pods carry both labels, so the same live IP set must appear under the
		// inbound service and the egress gateway.
		wantIPs, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wantIPs).To(HaveLen(dualPods), "expected %d live dual-traffic pod IPs", dualPods)
		wantList := make([]string, 0, len(wantIPs))
		for ip := range wantIPs {
			wantList = append(wantList, ip)
		}
		egressList := make([]string, 0, len(egressPodIPs))
		for ip := range egressPodIPs {
			egressList = append(egressList, ip)
		}
		inboundList := make([]string, 0, len(inboundPodIPs))
		for ip := range inboundPodIPs {
			inboundList = append(inboundList, ip)
		}
		Expect(egressList).To(ConsistOf(wantList),
			"the outbound service must register exactly the live dual-traffic pod IPs")
		Expect(inboundList).To(ConsistOf(wantList),
			"the inbound service must register exactly the live dual-traffic pod IPs")

		By("Verifying the SAME pod IPs are in both services")
		dualRegisteredCount := 0
		for ip := range egressPodIPs {
			if inboundPodIPs[ip] {
				dualRegisteredCount++
				utils.Logf("Pod IP %s is registered in BOTH inbound and outbound services", ip)
			}
		}

		Expect(dualRegisteredCount).To(Equal(dualPods), fmt.Sprintf("Expected %d pods in both services, got %d", dualPods, dualRegisteredCount))

		By("Deleting half of the dual-traffic pods")
		podsToDelete := dualPods / 2
		for i := 0; i < podsToDelete; i++ {
			err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), fmt.Sprintf("dual-pod-%d", i), metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for deletion to propagate")
		expectedRemaining := dualPods - podsToDelete
		Eventually(func() error {
			alResponse, err := queryServiceGatewayAddressLocations()
			if err != nil {
				return fmt.Errorf("query Service Gateway address locations: %w", err)
			}

			remainingEgress := 0
			remainingInbound := 0
			remainingDual := 0
			remainingEgressIPs := make(map[string]bool)
			remainingInboundIPs := make(map[string]bool)

			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == egressName {
							remainingEgress++
							remainingEgressIPs[addr.Address] = true
						}
						if svcName == serviceUID {
							remainingInbound++
							remainingInboundIPs[addr.Address] = true
						}
					}
				}
			}

			for ip := range remainingEgressIPs {
				if remainingInboundIPs[ip] {
					remainingDual++
				}
			}

			if remainingEgress != expectedRemaining {
				return fmt.Errorf("got %d pods in outbound after deletion, want %d", remainingEgress, expectedRemaining)
			}
			if remainingInbound != expectedRemaining {
				return fmt.Errorf("got %d pods in inbound after deletion, want %d", remainingInbound, expectedRemaining)
			}
			if remainingDual != expectedRemaining {
				return fmt.Errorf("got %d pods in both services after deletion, want %d", remainingDual, expectedRemaining)
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"remaining dual-traffic pods should stay registered in both services after deletion")

		By("Verifying remaining pods are still in both services")
		alResponse, err = queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		remainingEgress := 0
		remainingInbound := 0
		remainingDual := 0
		remainingEgressIPs := make(map[string]bool)
		remainingInboundIPs := make(map[string]bool)

		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						remainingEgress++
						remainingEgressIPs[addr.Address] = true
					}
					if svcName == serviceUID {
						remainingInbound++
						remainingInboundIPs[addr.Address] = true
					}
				}
			}
		}

		for ip := range remainingEgressIPs {
			if remainingInboundIPs[ip] {
				remainingDual++
			}
		}

		utils.Logf("After deletion: %d pods in outbound, %d pods in inbound, %d in both", remainingEgress, remainingInbound, remainingDual)

		Expect(remainingEgress).To(Equal(expectedRemaining), fmt.Sprintf("Expected %d pods in outbound after deletion, got %d", expectedRemaining, remainingEgress))
		Expect(remainingInbound).To(Equal(expectedRemaining), fmt.Sprintf("Expected %d pods in inbound after deletion, got %d", expectedRemaining, remainingInbound))
		Expect(remainingDual).To(Equal(expectedRemaining), fmt.Sprintf("Expected %d pods in both services after deletion, got %d", expectedRemaining, remainingDual))

		utils.Logf("\n✓ Dual inbound+outbound pod test passed: %d pods with both LB and NAT Gateway", dualPods)
	})

	It("should use a BYO NAT Gateway from the VNet resource group and only unlink it when the identity goes away", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-egress-" + ns.Name[len(ns.Name)-5:]

		By("Creating a BYO Public IP and NAT Gateway in the VNet resource group")
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Creating egress pods that name the BYO NAT Gateway")
		createEgressPods(cs, ns.Name, egressName, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for the identity to be registered with the BYO NAT Gateway")
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "the BYO NAT Gateway must be linked to the Service Gateway")
		eventuallyNamespaceEvent(cs, ns.Name, "ServiceGatewayEgressNATGatewayLinked", time.Minute)
		out, err := runAz("network", "nat", "gateway", "show", "-g", resourceGroupName, "-n", egressName)
		Expect(err).To(HaveOccurred(), "no managed NAT Gateway may be created for an identity served by a BYO NAT Gateway")
		Expect(string(out)).To(ContainSubstring("ResourceNotFound"))

		By("Checking egress SNAT when the environment carries traffic")
		observedIP := ""
		if out, _ := utils.RunKubectl(ns.Name, "exec", egressPodName(egressName, 0), "--", "/bin/sh", "-c", "curl -4 -s -m 10 ifconfig.me/ip || true"); out != "" {
			observedIP = ipv4Regexp.FindString(out)
		}
		if observedIP != "" {
			byoIPs, err := getNatGatewayPublicIPs(natGatewayID)
			Expect(err).NotTo(HaveOccurred())
			Expect(byoIPs).To(ContainElement(observedIP), "egress must leave through the BYO NAT Gateway's Public IP")
		}

		By("Deleting the egress pods and checking the BYO resources are only unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)

		By("Creating the pods again: the same BYO NAT Gateway must be linked again")
		createEgressPods(cs, ns.Name, egressName, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)

		if observedIP == "" {
			Skip("control plane verified; this environment carries no egress traffic, so SNAT through the BYO NAT Gateway was not observed")
		}
	})

	It("should reject a BYO NAT Gateway without a Public IP and use it once one is attached", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-fix-" + ns.Name[len(ns.Name)-5:]

		By("Creating a BYO NAT Gateway without a Public IP")
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, byoAddresses{})

		By("Creating egress pods that name it and waiting for the rejection")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		Eventually(func() bool {
			failIfBYONATGatewayUnreadable(cs, ns.Name, byoRG)
			return namespaceHasEvent(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected")
		}, 2*time.Minute, defaultPollInterval).Should(BeTrue(), "expected a ServiceGatewayEgressNATGatewayRejected event")
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "a rejected identity must not be registered")
		Expect(byoNATGatewayLink(natGatewayID)).To(BeEmpty(), "a rejected NAT Gateway must not be linked")

		By("Attaching a Public IP; the retry must now use the gateway")
		putBYONATGateway(byoRG, egressName, addresses)
		Eventually(func() error {
			failIfBYONATGatewayUnreadable(cs, ns.Name, byoRG)
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if err := egressRegisteredMatchErr(egressName, want); err != nil {
				return err
			}
			return outboundServiceUsesNATGatewayErr(egressName, natGatewayID)
		}, 8*time.Minute, defaultPollInterval).Should(Succeed(), "the retry backs off up to 30s and may pause for 5 minutes after repeated failures")

		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "the BYO NAT Gateway must be linked to the Service Gateway")

		By("Deleting the egress pod and checking the gateway is unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
	})
	It("should reject a BYO NAT Gateway that is not StandardV2", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-sku-" + ns.Name[len(ns.Name)-5:]

		By("Creating a Standard (not StandardV2) NAT Gateway in the VNet resource group")
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"Standard"}}`, clusterLocation()))

		By("Creating egress pods that name it: the identity must be rejected and nothing linked")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, byoRG, "SKU")
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "a rejected identity must not be registered")
		Expect(byoNATGatewayLink(natGatewayID)).To(BeEmpty(), "a rejected NAT Gateway must not be linked")
		expectNoManagedNATGateway(egressName)
	})

	It("should use a BYO NAT Gateway that has a Public IP prefix instead of a Public IP", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-prefix-" + ns.Name[len(ns.Name)-5:]

		By("Creating StandardV2 Public IP prefixes and a NAT Gateway that uses them")
		prefixes := byoAddresses{ipv4: createBYOPublicIPPrefix(byoRG, egressName+"-prefix", "IPv4", 31)}
		if clusterHasIPv6Node(cs) {
			prefixes.ipv6 = createBYOPublicIPPrefix(byoRG, egressName+"-prefix-v6", "IPv6", 127)
		}
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpPrefixes":%s,"publicIpPrefixesV6":%s}}`,
			clusterLocation(), subResourceRefs(prefixes.ipv4), subResourceRefs(prefixes.ipv6)))

		By("Creating egress pods that name it")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())))

		By("Deleting the pods: the gateway and prefix are only unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, prefixes)
	})

	It("should keep two BYO egress identities on their own NAT Gateways", func() {
		byoRG := byoNATGatewayResourceGroup()
		suffix := ns.Name[len(ns.Name)-5:]
		first, second := "byo-one-"+suffix, "byo-two-"+suffix
		firstAddresses := createBYOAddresses(cs, byoRG, first)
		firstID := createBYONATGateway(byoRG, first, firstAddresses)
		secondAddresses := createBYOAddresses(cs, byoRG, second)
		secondID := createBYONATGateway(byoRG, second, secondAddresses)

		By("Creating pods for both identities")
		createEgressPods(cs, ns.Name, first, 1)
		createEgressPods(cs, ns.Name, second, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, first, firstID, 3*time.Minute)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, 3*time.Minute)

		By("Deleting the first identity's pods: only its gateway is unlinked")
		deleteEgressPods(cs, ns.Name, first)
		eventuallyBYOUnlinked(first, firstID)
		expectBYOResourcesExist(firstID, firstAddresses)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(secondID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "the second gateway must stay linked")
	})

	It("should run BYO and managed egress identities side by side, through scaling and label moves", func() {
		byoRG := byoNATGatewayResourceGroup()
		suffix := ns.Name[len(ns.Name)-5:]
		byoName, managedName := "byo-mix-"+suffix, "managed-mix-"+suffix
		addresses := createBYOAddresses(cs, byoRG, byoName)
		natGatewayID := createBYONATGateway(byoRG, byoName, addresses)
		managedID := byoNATGatewayARMID(resourceGroupName, managedName)

		By("Creating pods for both identities")
		createEgressPods(cs, ns.Name, byoName, 1)
		createEgressPods(cs, ns.Name, managedName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 3*time.Minute)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)
		out, err := runAz("rest", "--method", "get", "--url", armURL(managedID), "--query", "tags", "-o", "json")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(string(out)).To(ContainSubstring(fmt.Sprintf(`"k8s-azure-egress-identity": %q`, managedName)))

		By("Scaling the BYO identity up to 3 pods and down to 1")
		createEgressPodsFrom(cs, ns.Name, byoName, 1, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 2*time.Minute)
		for _, i := range []int{1, 2} {
			Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), egressPodName(byoName, i), metav1.DeleteOptions{})).To(Succeed())
		}
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, byoName) }, 3*time.Minute, defaultPollInterval).Should(Equal(1))
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 2*time.Minute)

		By("Moving the last BYO pod to the managed identity: the BYO gateway is unlinked, the managed one gains the pod")
		patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, egressLabel, managedName)
		_, err = cs.CoreV1().Pods(ns.Name).Patch(context.TODO(), egressPodName(byoName, 0), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyBYOUnlinked(byoName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)
	})

	It("should never take over the NAT Gateway of the default outbound service", func() {
		defaultNATGatewayID := defaultOutboundNATGatewayID()
		defaultName := resourceNameFromID(defaultNATGatewayID)
		defaultRG := resourceGroupFromID(defaultNATGatewayID)
		switch {
		case defaultName == "" || strings.EqualFold(defaultName, "default-natgw"):
			Skip("the default outbound service uses the AKS NAT Gateway, whose name is reserved for egress identities")
		case len(validation.IsValidLabelValue(defaultName)) > 0:
			Skip(fmt.Sprintf("the default outbound NAT Gateway name %q is not a valid pod label value", defaultName))
		case !strings.EqualFold(defaultRG, resourceGroupName) && !strings.EqualFold(defaultRG, vnetResourceGroup()):
			Skip("the default outbound NAT Gateway is in neither the cluster nor the VNet resource group, so no egress identity can resolve to it")
		}
		defaultLink := byoNATGatewayLink(defaultNATGatewayID)

		By(fmt.Sprintf("Creating egress pods named after the default outbound NAT Gateway %q", defaultName))
		createEgressPods(cs, ns.Name, defaultName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, defaultRG, "")
		Expect(outboundServiceGoneErr(defaultName)).To(Succeed(), "the identity must not be registered")

		By("Deleting the pods and checking the default outbound service and its NAT Gateway are untouched")
		deleteEgressPods(cs, ns.Name, defaultName)
		Consistently(func() error {
			if got := defaultOutboundNATGatewayID(); !strings.EqualFold(got, defaultNATGatewayID) {
				return fmt.Errorf("the default outbound service now uses %q, want %q", got, defaultNATGatewayID)
			}
			if link := byoNATGatewayLink(defaultNATGatewayID); !strings.EqualFold(link, defaultLink) {
				return fmt.Errorf("the default outbound NAT Gateway is linked to %q, want %q", link, defaultLink)
			}
			return nil
		}, time.Minute, defaultPollInterval).Should(Succeed())
	})

	It("should never sweep or take over a NAT Gateway in the cluster resource group it did not create", func() {
		foreignName := "foreign-" + ns.Name[len(ns.Name)-5:]

		By("Creating an untagged NAT Gateway in the cluster resource group")
		foreignID := byoNATGatewayARMID(resourceGroupName, foreignName)
		DeferCleanup(deleteBYOResource, foreignID)
		putBYOResource(foreignID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"}}`, clusterLocation()))
		snapshot := natGatewayTagsAndLink(foreignID)
		Expect(snapshot).NotTo(ContainSubstring("k8s-azure-egress-identity"))
		expectUnchanged := func() {
			Consistently(func() (string, error) {
				_, err := runAz("rest", "--method", "get", "--url", armURL(foreignID))
				if err != nil {
					return "", err
				}
				return natGatewayTagsAndLink(foreignID), nil
			}, time.Minute, defaultPollInterval).Should(Equal(snapshot), "the NAT Gateway must be neither deleted nor modified")
		}

		By("Creating egress pods named after it while the controller is running: rejected by the live ownership check")
		createEgressPods(cs, ns.Name, foreignName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, resourceGroupName, "")
		Expect(outboundServiceGoneErr(foreignName)).To(Succeed(), "a rejected identity must not be registered")
		deleteEgressPods(cs, ns.Name, foreignName)
		expectUnchanged()

		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; the start-up part of this spec needs a cloud-controller-manager restart")
		}
		By("Restarting the cloud-controller-manager, whose start-up cleanup must leave it alone")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())
		expectUnchanged()

		By("Creating egress pods named after it again: rejected by the start-up record")
		clearNamespaceEvents(cs, ns.Name)
		createEgressPods(cs, ns.Name, foreignName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, resourceGroupName, "")
		deleteEgressPods(cs, ns.Name, foreignName)
		expectUnchanged()
	})

	It("should keep treating a BYO NAT Gateway as BYO after the controller restarts", func() {
		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; cannot restart the cloud-controller-manager")
		}
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-restart-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Registering an identity on the BYO NAT Gateway")
		createEgressPods(cs, ns.Name, egressName, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Restarting the cloud-controller-manager")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Deleting the pods: the restarted controller must only unlink the BYO NAT Gateway")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
		expectNoManagedNATGateway(egressName)
	})

	It("should not hold the last egress pod when a lock prevents unlinking the BYO NAT Gateway", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-lock-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Registering an identity on the BYO NAT Gateway")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Locking the BYO NAT Gateway read-only, then deleting the pod")
		lockID := natGatewayID + "/providers/Microsoft.Authorization/locks/byo-e2e-readonly"
		// Runs before the gateway's own cleanup: unlock, then clear the link the lock left behind.
		DeferCleanup(func() {
			_, _ = runAz("rest", "--method", "delete", "--url", "https://management.azure.com"+lockID+"?api-version=2016-09-01")
			if byoNATGatewayLink(natGatewayID) != "" {
				putBYONATGateway(byoRG, egressName, addresses) // a PUT without the link clears it
			}
		})
		out, err := runAz("rest", "--method", "put", "--url", "https://management.azure.com"+lockID+"?api-version=2016-09-01",
			"--body", `{"properties":{"level":"ReadOnly","notes":"cloud-provider-azure e2e"}}`)
		if err != nil && strings.Contains(string(out), "AuthorizationFailed") {
			Skip("the test identity cannot create resource locks (Microsoft.Authorization/locks/write); the lock handling is covered by unit tests")
		}
		Expect(err).NotTo(HaveOccurred(), string(out))
		deleteEgressPods(cs, ns.Name, egressName)

		By("The pod must go away and the identity be unregistered; the link is left behind")
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, egressName) }, 5*time.Minute, defaultPollInterval).Should(BeZero(),
			"a lock on the BYO NAT Gateway must not hold the pod in Terminating")
		Eventually(func() error { return outboundServiceGoneErr(egressName) }, 3*time.Minute, defaultPollInterval).Should(Succeed())
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())),
			"the locked gateway cannot be unlinked, so its link stays")
		expectBYOResourcesExist(natGatewayID, addresses)
	})

	It("should reject the reserved default-natgw label and leave the default outbound service alone", func() {
		defaultNATGatewayID := defaultOutboundNATGatewayID()
		Expect(defaultNATGatewayID).NotTo(BeEmpty(), "the cluster must have a default outbound service")

		By("Creating an egress pod labelled default-natgw")
		createEgressPods(cs, ns.Name, "default-natgw", 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyNamespaceEvent(cs, ns.Name, "ServiceGatewayReservedEgressLabel", 2*time.Minute)

		By("Deleting the pod: the default outbound service must be unchanged")
		deleteEgressPods(cs, ns.Name, "default-natgw")
		Consistently(func() string { return defaultOutboundNATGatewayID() }, time.Minute, defaultPollInterval).
			Should(Equal(defaultNATGatewayID), "the default outbound service must keep its NAT Gateway")
	})

	It("should use a BYO NAT Gateway that its owner already linked to the Service Gateway", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-prelinked-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":%s,"publicIpAddressesV6":%s,"serviceGateway":{"id":%q}}}`,
			clusterLocation(), subResourceRefs(addresses.ipv4), subResourceRefs(addresses.ipv6), serviceGatewayResourceID()))
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "precondition: linked by its owner")

		By("Creating egress pods that name it")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		expectNoManagedNATGateway(egressName)

		By("Deleting the pods: the link is removed like any other BYO link")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
	})

	It("should reject a BYO NAT Gateway linked to another Service Gateway and leave that link alone", func() {
		byoRG := byoNATGatewayResourceGroup()
		suffix := ns.Name[len(ns.Name)-5:]
		egressName := "byo-othersgw-" + suffix

		By("Creating another Service Gateway (with its own VNet) in the VNet resource group")
		otherVNetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/other-sgw-vnet-%s", subscriptionID, byoRG, suffix)
		DeferCleanup(deleteBYOResource, otherVNetID)
		putBYOResource(otherVNetID, fmt.Sprintf(`{"location":%q,"properties":{"addressSpace":{"addressPrefixes":["10.50.0.0/16"]}}}`, clusterLocation()))
		otherSGWID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/other-sgw-%s", subscriptionID, byoRG, suffix)
		DeferCleanup(deleteBYOResource, otherSGWID)
		putBYOResource(otherSGWID, fmt.Sprintf(`{"location":%q,"sku":{"name":"Standard","tier":"Regional"},"properties":{"virtualNetwork":{"id":%q}}}`, clusterLocation(), otherVNetID))

		By("Creating a BYO NAT Gateway linked to that other Service Gateway")
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		// Runs first: unlink it from the other Service Gateway so both can be deleted.
		DeferCleanup(func() { putBYONATGateway(byoRG, egressName, addresses) })
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":%s,"publicIpAddressesV6":%s,"serviceGateway":{"id":%q}}}`,
			clusterLocation(), subResourceRefs(addresses.ipv4), subResourceRefs(addresses.ipv6), otherSGWID))

		By("Creating egress pods that name it: rejected, and the other link kept")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, byoRG, "already linked to Service Gateway")
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "a rejected identity must not be registered")
		deleteEgressPods(cs, ns.Name, egressName)
		Consistently(func() string { return strings.ToLower(byoNATGatewayLink(natGatewayID)) }, time.Minute, defaultPollInterval).
			Should(Equal(strings.ToLower(otherSGWID)), "the link to the other Service Gateway must never be touched")
		expectNoManagedNATGateway(egressName)
	})

	It("should reuse and later delete a tagged managed NAT Gateway already in the cluster resource group", func() {
		egressName := "managed-tagged-" + ns.Name[len(ns.Name)-5:]

		By("Creating a NAT Gateway and Public IP tagged for the identity in the cluster resource group")
		pipID := createClusterPublicIP(PublicIPNameForEgress(egressName), egressName)
		natGatewayID := byoNATGatewayARMID(resourceGroupName, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":{"k8s-azure-egress-identity":%q},"properties":{"publicIpAddresses":[{"id":%q}]}}`,
			clusterLocation(), egressName, pipID))

		By("Creating egress pods: the identity must use it as its managed NAT Gateway")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyManagedRegistered(cs, ns.Name, egressName, natGatewayID)

		By("Deleting the pods: the controller owns it and deletes it")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyResourcesDeleted(natGatewayID, pipID)
	})

	It("should adopt a NAT Gateway created before ownership tags and later delete it", func() {
		egressName := "managed-legacy-" + ns.Name[len(ns.Name)-5:]

		By("Creating an untagged NAT Gateway whose only Public IP is its own <name>-pip, as older controllers did")
		pipID := createClusterPublicIP(PublicIPNameForEgress(egressName), "")
		natGatewayID := byoNATGatewayARMID(resourceGroupName, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":[{"id":%q}]}}`, clusterLocation(), pipID))

		By("Creating egress pods: the identity must use it and tag it")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyManagedRegistered(cs, ns.Name, egressName, natGatewayID)
		Eventually(func() string { return natGatewayTagsAndLink(natGatewayID) }, 2*time.Minute, defaultPollInterval).
			Should(ContainSubstring(fmt.Sprintf(`"k8s-azure-egress-identity": %q`, egressName)))

		By("Deleting the pods: the controller deletes it")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyResourcesDeleted(natGatewayID, pipID)
	})

	It("should move pods between BYO and managed egress identities", func() {
		byoRG := byoNATGatewayResourceGroup()
		suffix := ns.Name[len(ns.Name)-5:]
		first, second, managedName := "byo-move-a-"+suffix, "byo-move-b-"+suffix, "managed-move-"+suffix
		firstAddresses := createBYOAddresses(cs, byoRG, first)
		firstID := createBYONATGateway(byoRG, first, firstAddresses)
		secondAddresses := createBYOAddresses(cs, byoRG, second)
		secondID := createBYONATGateway(byoRG, second, secondAddresses)
		managedID := byoNATGatewayARMID(resourceGroupName, managedName)

		createEgressPods(cs, ns.Name, first, 1)
		createEgressPods(cs, ns.Name, second, 1)
		createEgressPods(cs, ns.Name, managedName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, first, firstID, 3*time.Minute)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, 3*time.Minute)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)

		By("Moving the first BYO identity's only pod to the second: the first gateway is unlinked")
		relabelEgressPod(cs, ns.Name, egressPodName(first, 0), second)
		eventuallyBYOUnlinked(first, firstID)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, 3*time.Minute)

		By("Moving the managed identity's only pod to the first BYO identity: the managed gateway is deleted, the BYO one linked again")
		relabelEgressPod(cs, ns.Name, egressPodName(managedName, 0), first)
		eventuallyResourcesDeleted(managedID)
		eventuallyBYORegistered(cs, ns.Name, byoRG, first, firstID, 3*time.Minute)
		expectBYOResourcesExist(firstID, firstAddresses)
		expectBYOResourcesExist(secondID, secondAddresses)
	})

	It("should keep a BYO identity on its NAT Gateway when its last pod is replaced at once", func() {
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-replace-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Deleting the only pod and creating a replacement straight away")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), egressPodName(egressName, 0), metav1.DeleteOptions{})).To(Succeed())
		createEgressPodsFrom(cs, ns.Name, egressName, 1, 1)
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, egressName) }, 3*time.Minute, defaultPollInterval).Should(Equal(1))
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 5*time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())))
		expectNoManagedNATGateway(egressName)
	})

	It("should unlink a BYO NAT Gateway whose pods were deleted while the controller was down", func() {
		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; cannot stop the cloud-controller-manager")
		}
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-down-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)
		createEgressPods(cs, ns.Name, egressName, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Stopping the cloud-controller-manager and deleting the pods while it is down")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		ccmToRestore = ccmClient
		Expect(scaleCCMDeployment(context.TODO(), ccmClient, 0)).To(Succeed())
		Expect(waitForCCMFullyDown(context.TODO(), ccmClient, GetCCMRecoveryTimeout())).To(Succeed())
		deleteEgressPods(cs, ns.Name, egressName)
		Consistently(func() (int, error) { return livePodCount(cs, ns.Name, egressName) }, 30*time.Second, 5*time.Second).
			Should(Equal(2), "the cleanup finalizer must hold the pods while the controller is down")

		By("Starting the cloud-controller-manager: it must only unlink the BYO NAT Gateway and release the pods")
		Expect(scaleCCMDeployment(context.TODO(), ccmClient, 1)).To(Succeed())
		Expect(waitForCCMFullyUp(context.TODO(), ccmClient, GetCCMRecoveryTimeout())).To(Succeed())
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, egressName) }, 6*time.Minute, defaultPollInterval).Should(BeZero())
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
		expectNoManagedNATGateway(egressName)
	})

	It("should sweep only its own orphaned NAT Gateways at start-up", func() {
		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; cannot restart the cloud-controller-manager")
		}
		suffix := ns.Name[len(ns.Name)-5:]
		tagged, legacy, foreign := "orphan-tagged-"+suffix, "orphan-legacy-"+suffix, "orphan-foreign-"+suffix

		By("Creating unreferenced NAT Gateways in the cluster resource group: tagged, pre-tagging, and foreign")
		taggedPIP := createClusterPublicIP(PublicIPNameForEgress(tagged), tagged)
		taggedID := byoNATGatewayARMID(resourceGroupName, tagged)
		DeferCleanup(deleteBYOResource, taggedID)
		putBYOResource(taggedID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":{"k8s-azure-egress-identity":%q},"properties":{"publicIpAddresses":[{"id":%q}]}}`,
			clusterLocation(), tagged, taggedPIP))
		legacyPIP := createClusterPublicIP(PublicIPNameForEgress(legacy), "")
		legacyID := byoNATGatewayARMID(resourceGroupName, legacy)
		DeferCleanup(deleteBYOResource, legacyID)
		putBYOResource(legacyID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":[{"id":%q}]}}`, clusterLocation(), legacyPIP))
		foreignID := byoNATGatewayARMID(resourceGroupName, foreign)
		DeferCleanup(deleteBYOResource, foreignID)
		putBYOResource(foreignID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"}}`, clusterLocation()))
		foreignSnapshot := natGatewayTagsAndLink(foreignID)

		By("Restarting the cloud-controller-manager")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())

		By("Its own orphans are deleted; the foreign NAT Gateway is kept")
		eventuallyResourcesDeleted(taggedID, taggedPIP, legacyID, legacyPIP)
		Consistently(func() (string, error) {
			if _, err := runAz("rest", "--method", "get", "--url", armURL(foreignID)); err != nil {
				return "", err
			}
			return natGatewayTagsAndLink(foreignID), nil
		}, 2*time.Minute, defaultPollInterval).Should(Equal(foreignSnapshot), "a NAT Gateway the controller did not create must never be swept or changed")
	})

	It("should handle missing write access to BYO NAT Gateways without holding pods", func() {
		ids := byoAccessTestIdentities()
		byoRG := byoNATGatewayResourceGroup()
		suffix := ns.Name[len(ns.Name)-5:]
		inUse := "byo-acc-used-" + suffix
		inUseAddresses := createBYOAddresses(cs, byoRG, inUse)
		inUseID := createBYONATGateway(byoRG, inUse, inUseAddresses)

		By("Registering an identity while the cluster identity has write access")
		createEgressPods(cs, ns.Name, inUse, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, inUse, inUseID, 3*time.Minute)

		By("Reducing the cluster identity to Reader on the VNet resource group")
		restore := replaceClusterIdentityRoles(ids, byoRG, "Reader")
		DeferCleanup(func() { // the unlink the controller could not do
			if byoNATGatewayLink(inUseID) != "" {
				putBYONATGateway(byoRG, inUse, inUseAddresses)
			}
		})

		By("Probing with new identities until one is rejected with access denied when linking")
		var probe, probeID string
		untilRoleChangeApplies(func(attempt int) bool {
			probe = fmt.Sprintf("byo-acc-probe-%s-%d", suffix, attempt)
			probeAddresses := createBYOAddresses(cs, byoRG, probe)
			probeID = createBYONATGateway(byoRG, probe, probeAddresses)
			name, id := probe, probeID
			DeferCleanup(func() { // a probe linked before the change applied, then denied its unlink
				if byoNATGatewayLink(id) != "" {
					putBYONATGateway(byoRG, name, probeAddresses)
				}
			})
			createEgressPods(cs, ns.Name, probe, 1)
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			denied := false
			Eventually(func() bool {
				denied = namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected",
					fmt.Sprintf("Egress identity %q cannot use NAT Gateway", name), "access denied when linking")
				return denied || byoRegisteredErr(cs, ns.Name, name, id) == nil
			}, 3*time.Minute, defaultPollInterval).Should(BeTrue())
			if !denied {
				deleteEgressPods(cs, ns.Name, name) // linked before the change applied; try a new one
				Eventually(func() error { return outboundServiceGoneErr(name) }, 6*time.Minute, defaultPollInterval).Should(Succeed())
			}
			return denied
		})
		Expect(outboundServiceGoneErr(probe)).To(Succeed())

		By("Deleting the registered identity's pod: it must go away although the unlink is denied")
		deleteEgressPods(cs, ns.Name, inUse)
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, inUse) }, 6*time.Minute, defaultPollInterval).Should(BeZero(),
			"missing write access must not hold the pod in Terminating")
		Eventually(func() error { return outboundServiceGoneErr(inUse) }, 3*time.Minute, defaultPollInterval).Should(Succeed())
		Expect(strings.ToLower(byoNATGatewayLink(inUseID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "the denied unlink leaves the link")

		By("Restoring write access: the rejected identity is picked up without restarting its pod")
		restore()
		// No fail-fast here: the earlier access-denied events stay, and retries emit more until the
		// role change applies.
		Eventually(func() error { return byoRegisteredErr(cs, ns.Name, probe, probeID) }, 20*time.Minute, defaultPollInterval).Should(Succeed())
		deleteEgressPods(cs, ns.Name, probe)
		eventuallyBYOUnlinked(probe, probeID)
	})

	It("should fall back to a managed NAT Gateway when the VNet resource group cannot be read", func() {
		ids := byoAccessTestIdentities()
		byoRG := byoNATGatewayResourceGroup()
		egressName := "byo-noread-" + ns.Name[len(ns.Name)-5:]
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)
		managedID := byoNATGatewayARMID(resourceGroupName, egressName)

		By("Removing the cluster identity's access to the VNet resource group")
		replaceClusterIdentityRoles(ids, byoRG, "")

		By("Probing with new identities (no BYO NAT Gateway of their name) until one reports the VNet resource group unreadable")
		untilRoleChangeApplies(func(attempt int) bool {
			probe := fmt.Sprintf("byo-noread-probe-%s-%d", ns.Name[len(ns.Name)-5:], attempt)
			createEgressPods(cs, ns.Name, probe, 1)
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			// Either way the probe gets a managed NAT Gateway; only the event tells whether the read was denied.
			eventuallyManagedRegistered(cs, ns.Name, probe, byoNATGatewayARMID(resourceGroupName, probe))
			unreadable := namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayUnreadable", fmt.Sprintf("egress identity %q", probe))
			deleteEgressPods(cs, ns.Name, probe)
			eventuallyResourcesDeleted(byoNATGatewayARMID(resourceGroupName, probe))
			return unreadable
		})

		By("Creating egress pods for the BYO NAT Gateway: they must egress through a managed one, with a Normal event explaining why")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		Eventually(func() bool {
			return namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayUnreadable", fmt.Sprintf("egress identity %q", egressName))
		}, 3*time.Minute, defaultPollInterval).Should(BeTrue())
		eventuallyManagedRegistered(cs, ns.Name, egressName, managedID)
		Expect(byoNATGatewayLink(natGatewayID)).To(BeEmpty(), "the unreadable BYO NAT Gateway must not be linked")

		By("Deleting the pods: the managed NAT Gateway is deleted, the BYO one untouched")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyResourcesDeleted(managedID)
		expectBYOResourcesExist(natGatewayID, addresses)
	})
})

// livePodIPsWithLabel returns the address set the pods carrying labelKey=labelValue should have
// registered. Egress specs compare against this set rather than a count: after a partial deletion
// only the set can distinguish the surviving pods from the deleted ones.
func livePodIPsWithLabel(cs clientset.Interface, namespace, labelKey, labelValue string) (map[string]struct{}, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{labelKey: labelValue}).String(),
	})
	if err != nil {
		return nil, err
	}
	ready := make([]v1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			ready = append(ready, pods.Items[i])
		}
	}
	return podIPSet(ready), nil
}

// byoResourceAPIVersion supports StandardV2 NAT Gateways and Public IPs.
const byoResourceAPIVersion = "2025-05-01"

// vnetResourceGroup returns the resource group of the cluster's VNet, read from a node scale set's
// subnet, or "" when it cannot be determined.
func vnetResourceGroup() string {
	out, err := runAz("vmss", "list", "-g", resourceGroupName, "--query",
		"[0].virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id", "-o", "tsv")
	Expect(err).NotTo(HaveOccurred(), "listing node scale sets: %s", string(out))
	return resourceGroupFromID(strings.TrimSpace(string(out)))
}

// byoNATGatewayResourceGroup returns the VNet resource group for the BYO NAT Gateway specs, which
// only run when it differs from the cluster resource group (a BYO-VNet cluster); otherwise they skip,
// saying why.
func byoNATGatewayResourceGroup() string {
	rg := vnetResourceGroup()
	if rg == "" {
		Skip("could not determine the cluster VNet's resource group (no node scale set with a subnet)")
	}
	if strings.EqualFold(rg, resourceGroupName) {
		Skip("the cluster VNet is not in a separate resource group, so BYO NAT Gateways are not looked up")
	}
	return rg
}

var resourceGroupInIDRegexp = regexp.MustCompile(`(?i)/resourceGroups/([^/]+)/`)

func armURL(resourceID string) string {
	return fmt.Sprintf("https://management.azure.com%s?api-version=%s", resourceID, byoResourceAPIVersion)
}

func serviceGatewayResourceID() string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s", subscriptionID, resourceGroupName, serviceGatewayName)
}

func clusterLocation() string {
	out, err := runAz("group", "show", "-n", resourceGroupName, "--query", "location", "-o", "tsv")
	Expect(err).NotTo(HaveOccurred(), string(out))
	return strings.TrimSpace(string(out))
}

// deleteBYOResource deletes an ARM resource and waits for the deletion to finish.
func deleteBYOResource(resourceID string) {
	out, err := runAz("resource", "delete", "--ids", resourceID)
	Expect(err).NotTo(HaveOccurred(), "deleting %s: %s", resourceID, string(out))
}

// putBYOResource PUTs an ARM resource and waits until it is provisioned.
func putBYOResource(resourceID, body string) {
	out, err := runAz("rest", "--method", "put", "--url", armURL(resourceID), "--body", body)
	Expect(err).NotTo(HaveOccurred(), string(out))
	Eventually(func() (string, error) {
		out, err := runAz("rest", "--method", "get", "--url", armURL(resourceID), "--query", "properties.provisioningState", "-o", "tsv")
		return strings.TrimSpace(string(out)), err
	}, 3*time.Minute, defaultPollInterval).Should(Equal("Succeeded"), "resource %s should be provisioned", resourceID)
}

// byoAddresses are the BYO Public IPs of a BYO NAT Gateway; ipv6 is set only on clusters with IPv6
// nodes, where egress identities need both families.
type byoAddresses struct{ ipv4, ipv6 string }

// createBYOAddresses creates the StandardV2 Public IPs a BYO NAT Gateway needs on this cluster.
func createBYOAddresses(cs clientset.Interface, rg, name string) byoAddresses {
	addresses := byoAddresses{ipv4: createBYOPublicIP(rg, name+"-pip", "IPv4")}
	if clusterHasIPv6Node(cs) {
		addresses.ipv6 = createBYOPublicIP(rg, name+"-pip-v6", "IPv6")
	}
	return addresses
}

// createBYOPublicIP creates a StandardV2 Public IP that is deleted after the spec.
func createBYOPublicIP(rg, name, version string) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", subscriptionID, rg, name)
	// Registered first, so a failed create is still cleaned up (deleting a missing resource succeeds).
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIPAllocationMethod":"Static","publicIPAddressVersion":%q}}`, clusterLocation(), version))
	return id
}

// createBYONATGateway creates a StandardV2 NAT Gateway with the given addresses (none when empty)
// that is deleted after the spec, before its Public IPs, as DeferCleanup runs in reverse order.
func createBYONATGateway(rg, name string, addresses byoAddresses) string {
	// Registered first, so a failed create is still cleaned up (deleting a missing resource succeeds).
	DeferCleanup(deleteBYOResource, byoNATGatewayARMID(rg, name))
	return putBYONATGateway(rg, name, addresses)
}

func byoNATGatewayARMID(rg, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s", subscriptionID, rg, name)
}

func putBYONATGateway(rg, name string, addresses byoAddresses) string {
	id := byoNATGatewayARMID(rg, name)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":%s,"publicIpAddressesV6":%s}}`,
		clusterLocation(), subResourceRefs(addresses.ipv4), subResourceRefs(addresses.ipv6)))
	return id
}

// byoNATGatewayLink returns the Service Gateway a NAT Gateway is linked to, or "".
func byoNATGatewayLink(natGatewayID string) string {
	out, err := runAz("rest", "--method", "get", "--url", armURL(natGatewayID), "--query", "properties.serviceGateway.id", "-o", "tsv")
	Expect(err).NotTo(HaveOccurred(), string(out))
	return strings.TrimSpace(string(out))
}

func createEgressPods(cs clientset.Interface, namespace, egressName string, count int) {
	createEgressPodsFrom(cs, namespace, egressName, 0, count)
}

// createEgressPodsFrom creates count egress pods for egressName, numbered from first.
func createEgressPodsFrom(cs clientset.Interface, namespace, egressName string, first, count int) {
	for i := first; i < first+count; i++ {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: egressPodName(egressName, i), Namespace: namespace, Labels: map[string]string{egressLabel: egressName}},
			Spec: v1.PodSpec{Containers: []v1.Container{{
				Name: "test-app", Image: utils.AgnhostImage, ImagePullPolicy: v1.PullIfNotPresent, Args: []string{"netexec", "--http-port=8080"},
			}}},
		}
		_, err := cs.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
}

// outboundServiceUsesNATGatewayErr returns nil once the egress identity is registered with natGatewayID.
func outboundServiceUsesNATGatewayErr(egressName, natGatewayID string) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return err
	}
	for _, svc := range sgResponse.Value {
		if svc.Name == egressName {
			if !strings.EqualFold(svc.Properties.PublicNatGatewayID, natGatewayID) {
				return fmt.Errorf("egress %s uses NAT Gateway %q, want %q", egressName, svc.Properties.PublicNatGatewayID, natGatewayID)
			}
			return nil
		}
	}
	return fmt.Errorf("egress %s is not registered", egressName)
}

// outboundServiceGoneErr returns nil when the egress identity is not registered.
func outboundServiceGoneErr(egressName string) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return err
	}
	for _, svc := range sgResponse.Value {
		if svc.Name == egressName {
			return fmt.Errorf("egress %s is still registered", egressName)
		}
	}
	return nil
}

// namespaceHasEvent reports whether namespace has an event with the given reason.
func namespaceHasEvent(cs clientset.Interface, namespace, reason string) bool {
	events, err := cs.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false
	}
	for _, e := range events.Items {
		if e.Reason == reason {
			return true
		}
	}
	return false
}

// eventuallyNamespaceEvent waits for an event with the given reason in namespace.
func eventuallyNamespaceEvent(cs clientset.Interface, namespace, reason string, timeout time.Duration) {
	Eventually(func() bool { return namespaceHasEvent(cs, namespace, reason) }, timeout, defaultPollInterval).
		Should(BeTrue(), "expected a %s event in namespace %s", reason, namespace)
}

// failIfBYONATGatewayUnreadable stops a BYO spec at once when the controller reported that the cluster
// identity cannot read, or cannot link, NAT Gateways in the VNet resource group: the BYO behaviour
// cannot be observed until it is granted read and write access to them.
func failIfBYONATGatewayUnreadable(cs clientset.Interface, namespace, byoRG string) {
	events, err := cs.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, e := range events.Items {
		if e.Reason == "ServiceGatewayEgressNATGatewayUnreadable" ||
			(e.Reason == "ServiceGatewayEgressNATGatewayRejected" && strings.Contains(e.Message, "access denied")) {
			Fail(fmt.Sprintf("the cluster identity lacks access to NAT Gateways in resource group %s (%s); grant it read and write access to them to run this spec", byoRG, e.Message))
		}
	}
}

// eventuallyBYORegistered waits until egressName is registered with the BYO NAT Gateway natGatewayID
// for all its live pods and the gateway is linked to the Service Gateway. It fails at once when the
// controller reports it cannot read or link BYO NAT Gateways.
func eventuallyBYORegistered(cs clientset.Interface, namespace, byoRG, egressName, natGatewayID string, timeout time.Duration) {
	Eventually(func() error {
		failIfBYONATGatewayUnreadable(cs, namespace, byoRG)
		return byoRegisteredErr(cs, namespace, egressName, natGatewayID)
	}, timeout, defaultPollInterval).Should(Succeed())
}

// byoRegisteredErr returns nil once egressName is registered with natGatewayID for all its live pods
// and the gateway is linked to the Service Gateway.
func byoRegisteredErr(cs clientset.Interface, namespace, egressName, natGatewayID string) error {
	want, err := livePodIPsWithLabel(cs, namespace, egressLabel, egressName)
	if err != nil {
		return err
	}
	if err := egressRegisteredMatchErr(egressName, want); err != nil {
		return err
	}
	if err := outboundServiceUsesNATGatewayErr(egressName, natGatewayID); err != nil {
		return err
	}
	if link := byoNATGatewayLink(natGatewayID); !strings.EqualFold(link, serviceGatewayResourceID()) {
		return fmt.Errorf("BYO NAT Gateway is linked to %q, want %q", link, serviceGatewayResourceID())
	}
	return nil
}

// expectBYOResourcesExist checks a BYO NAT Gateway and its Public IPs still exist.
func expectBYOResourcesExist(natGatewayID string, addresses byoAddresses) {
	for _, id := range []string{natGatewayID, addresses.ipv4, addresses.ipv6} {
		if id != "" {
			_, err := runAz("rest", "--method", "get", "--url", armURL(id))
			Expect(err).NotTo(HaveOccurred(), "BYO resource %s must survive the identity's deletion", id)
		}
	}
}

// defaultOutboundNATGatewayID returns the NAT Gateway ID of the default outbound service, or "".
func defaultOutboundNATGatewayID() string {
	sgResponse, err := queryServiceGatewayServices()
	Expect(err).NotTo(HaveOccurred())
	for _, svc := range sgResponse.Value {
		if svc.Properties.IsDefault || svc.Name == "default-natgw" {
			return svc.Properties.PublicNatGatewayID
		}
	}
	return ""
}

// resourceNameFromID returns the last segment of an ARM resource ID.
func resourceNameFromID(id string) string {
	return id[strings.LastIndex(id, "/")+1:]
}

// egressPodName returns a valid pod name for the i-th pod of an egress identity, whose name may
// contain characters or a length a pod name cannot have.
func egressPodName(egressName string, i int) string {
	base := strings.Trim(podNameInvalidChars.ReplaceAllString(strings.ToLower(egressName), "-"), "-")
	if len(base) > 50 {
		base = base[:50]
	}
	return fmt.Sprintf("egress-%s-%d", base, i)
}

var podNameInvalidChars = regexp.MustCompile(`[^a-z0-9-]+`)

// deleteEgressPods deletes every pod of an egress identity.
func deleteEgressPods(cs clientset.Interface, namespace, egressName string) {
	Expect(cs.CoreV1().Pods(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: egressLabel + "=" + egressName})).To(Succeed())
}

// livePodCount returns how many pods of an egress identity still exist (including terminating ones).
func livePodCount(cs clientset.Interface, namespace, egressName string) (int, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: egressLabel + "=" + egressName})
	if err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

// eventuallyBYOUnlinked waits until an egress identity is unregistered and its BYO NAT Gateway unlinked.
func eventuallyBYOUnlinked(egressName, natGatewayID string) {
	Eventually(func() error {
		if err := outboundServiceGoneErr(egressName); err != nil {
			return err
		}
		if link := byoNATGatewayLink(natGatewayID); link != "" {
			return fmt.Errorf("BYO NAT Gateway is still linked to %s", link)
		}
		return nil
	}, 6*time.Minute, defaultPollInterval).Should(Succeed())
}

// eventuallyManagedRegistered waits until a managed egress identity is registered with its NAT Gateway
// in the cluster resource group for all its live pods.
func eventuallyManagedRegistered(cs clientset.Interface, namespace, egressName, natGatewayID string) {
	Eventually(func() error {
		want, err := livePodIPsWithLabel(cs, namespace, egressLabel, egressName)
		if err != nil {
			return err
		}
		if err := egressRegisteredMatchErr(egressName, want); err != nil {
			return err
		}
		return outboundServiceUsesNATGatewayErr(egressName, natGatewayID)
	}, 3*time.Minute, defaultPollInterval).Should(Succeed(), "the managed identity must get a NAT Gateway in the cluster resource group")
}

// eventuallyRejected waits for a ServiceGatewayEgressNATGatewayRejected event whose message contains
// want (any message when want is empty).
func eventuallyRejected(cs clientset.Interface, namespace, rg, want string) {
	Eventually(func() bool {
		failIfBYONATGatewayUnreadable(cs, namespace, rg)
		events, err := cs.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, e := range events.Items {
			if e.Reason == "ServiceGatewayEgressNATGatewayRejected" && e.Type == v1.EventTypeWarning && strings.Contains(e.Message, want) {
				return true
			}
		}
		return false
	}, 2*time.Minute, defaultPollInterval).Should(BeTrue(), "expected a ServiceGatewayEgressNATGatewayRejected Warning containing %q", want)
}

// clearNamespaceEvents deletes the events in namespace, so a later wait sees only new ones.
func clearNamespaceEvents(cs clientset.Interface, namespace string) {
	Expect(cs.CoreV1().Events(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
}

// expectNoManagedNATGateway checks no managed NAT Gateway exists for an egress identity.
func expectNoManagedNATGateway(egressName string) {
	out, err := runAz("network", "nat", "gateway", "show", "-g", resourceGroupName, "-n", egressName)
	Expect(err).To(HaveOccurred(), "no managed NAT Gateway may be created for %s", egressName)
	Expect(string(out)).To(ContainSubstring("ResourceNotFound"))
}

// natGatewayTagsAndLink returns a NAT Gateway's tags and Service Gateway link, as JSON.
func natGatewayTagsAndLink(natGatewayID string) string {
	out, err := runAz("rest", "--method", "get", "--url", armURL(natGatewayID), "--query", "{tags:tags,link:properties.serviceGateway.id}", "-o", "json")
	Expect(err).NotTo(HaveOccurred(), string(out))
	return string(out)
}

// resourceGroupFromID returns the resource group of an ARM resource ID, or "".
func resourceGroupFromID(id string) string {
	if match := resourceGroupInIDRegexp.FindStringSubmatch(id); match != nil {
		return match[1]
	}
	return ""
}

// subResourceRefs returns a JSON list referencing id, or an empty list when id is "".
func subResourceRefs(id string) string {
	if id == "" {
		return "[]"
	}
	return fmt.Sprintf(`[{"id":%q}]`, id)
}

// createBYOPublicIPPrefix creates a StandardV2 Public IP prefix that is deleted after the spec.
func createBYOPublicIPPrefix(rg, name, version string, length int) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPPrefixes/%s", subscriptionID, rg, name)
	// Registered first, so a failed create is still cleaned up (deleting a missing resource succeeds).
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"prefixLength":%d,"publicIPAddressVersion":%q}}`,
		clusterLocation(), length, version))
	return id
}

// byoAccessIdentitiesEnvVar lists the object IDs of the identities the cloud-controller-manager uses,
// comma separated. The access specs change their role on the VNet resource group, so they only run
// when it is set.
const byoAccessIdentitiesEnvVar = "BYO_E2E_CLUSTER_IDENTITY_OBJECT_IDS"

func byoAccessTestIdentities() []string {
	var ids []string
	for _, id := range strings.Split(os.Getenv(byoAccessIdentitiesEnvVar), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		Skip(byoAccessIdentitiesEnvVar + " is not set; this spec changes the cluster identity's role on the VNet resource group")
	}
	return ids
}

// replaceClusterIdentityRoles replaces each identity's role assignments on the resource group with
// role ("" for none). The original assignments are restored at cleanup, or earlier by calling the
// returned function. It skips the spec when the identities get access in another way (a role granted
// above the resource group), because removing the resource-group assignments would then not change
// what they can do.
func replaceClusterIdentityRoles(ids []string, rg, role string) (restore func()) {
	scope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscriptionID, rg)
	original := map[string][]string{}
	for _, id := range ids {
		out, err := runAz("role", "assignment", "list", "--assignee", id, "--all", "--query", "[].{role:roleDefinitionName,scope:scope}", "-o", "json")
		Expect(err).NotTo(HaveOccurred(), string(out))
		var assignments []struct{ Role, Scope string }
		Expect(json.Unmarshal(out, &assignments)).To(Succeed())
		for _, a := range assignments {
			switch {
			case strings.EqualFold(a.Scope, scope):
				original[id] = append(original[id], a.Role)
			case strings.HasPrefix(strings.ToLower(scope), strings.ToLower(a.Scope)+"/"):
				Skip(fmt.Sprintf("identity %s has %q on %s, which also grants access to %s", id, a.Role, a.Scope, rg))
			}
		}
	}
	assign := func(roles map[string][]string) {
		for _, id := range ids {
			// Create first, then remove the rest, so the identity is never left with no role at all.
			for _, r := range roles[id] {
				out, err := runAz("role", "assignment", "create", "--assignee-object-id", id, "--assignee-principal-type", "ServicePrincipal", "--role", r, "--scope", scope)
				Expect(err).NotTo(HaveOccurred(), string(out))
			}
			for _, r := range append([]string{"Reader", "Network Contributor"}, original[id]...) {
				if !slices.Contains(roles[id], r) {
					_, _ = runAz("role", "assignment", "delete", "--assignee", id, "--role", r, "--scope", scope)
				}
			}
		}
	}
	target := map[string][]string{}
	for _, id := range ids {
		if role != "" {
			target[id] = []string{role}
		}
	}
	restored := false
	restore = func() {
		if !restored {
			restored = true
			assign(original)
		}
	}
	DeferCleanup(restore)
	assign(target)
	return restore
}

// relabelEgressPod moves a pod to another egress identity.
func relabelEgressPod(cs clientset.Interface, namespace, podName, egressName string) {
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, egressLabel, egressName)
	_, err := cs.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

// createClusterPublicIP creates a StandardV2 Public IP in the cluster resource group, tagged for
// egressIdentity unless it is "", that is deleted after the spec unless the controller deletes it first.
func createClusterPublicIP(name, egressIdentity string) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", subscriptionID, resourceGroupName, name)
	tags := "{}"
	if egressIdentity != "" {
		tags = fmt.Sprintf(`{"k8s-azure-egress-identity":%q}`, egressIdentity)
	}
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":%s,"properties":{"publicIPAllocationMethod":"Static","publicIPAddressVersion":"IPv4"}}`,
		clusterLocation(), tags))
	return id
}

// PublicIPNameForEgress mirrors the controller's naming of an egress identity's IPv4 Public IP.
func PublicIPNameForEgress(egressName string) string {
	return egressName + "-pip"
}

// eventuallyResourcesDeleted waits until every resource is gone.
func eventuallyResourcesDeleted(ids ...string) {
	Eventually(func() error {
		for _, id := range ids {
			out, err := runAz("rest", "--method", "get", "--url", armURL(id))
			if err == nil {
				return fmt.Errorf("%s still exists", id)
			}
			if !strings.Contains(string(out), "NotFound") {
				return fmt.Errorf("reading %s: %s", id, string(out))
			}
		}
		return nil
	}, 8*time.Minute, defaultPollInterval).Should(Succeed())
}

// namespaceHasEventContaining reports whether namespace has an event with the given reason whose
// message contains every one of wants.
func namespaceHasEventContaining(cs clientset.Interface, namespace, reason string, wants ...string) bool {
	events, err := cs.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false
	}
	for _, e := range events.Items {
		if e.Reason != reason {
			continue
		}
		matches := true
		for _, want := range wants {
			matches = matches && strings.Contains(e.Message, want)
		}
		if matches {
			return true
		}
	}
	return false
}

// untilRoleChangeApplies runs attempt with 0, 1, 2, ... until it reports that a role change on the
// cluster identity has taken effect. Azure can take several minutes to apply one, and the controller
// resolves an identity's NAT Gateway only once, so every attempt must use a new identity.
func untilRoleChangeApplies(attempt func(i int) bool) {
	deadline := time.Now().Add(20 * time.Minute)
	for i := 0; ; i++ {
		if attempt(i) {
			return
		}
		if time.Now().After(deadline) {
			Fail("the role change on the cluster identity did not take effect within 20 minutes")
		}
	}
}
