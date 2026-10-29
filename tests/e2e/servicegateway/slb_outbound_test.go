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
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

const (
	// egressLabel is the label key used to designate pods for outbound NAT gateway
	egressLabel = "kubernetes.azure.com/service-egress-gateway"

	byoNATGatewayResourceGroupAnnotation = "kubernetes.azure.com/service-egress-gateway-resource-group"
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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != numPods {
				return fmt.Errorf("expected %d active egress pods, got %d", numPods, podCount)
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
			if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
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
		Expect(natGatewayName).To(Equal(outboundServiceName(egressName)))

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
		Expect(resourceNameFromID(props.PublicIPAddresses[0].ID)).To(Equal(PublicIPNameForEgress(egressName)))
		pipOutput, err := runAz("rest", "--method", "get", "--url", armURL(props.PublicIPAddresses[0].ID), "--query", "tags", "-o", "json")
		Expect(err).NotTo(HaveOccurred())
		var pipTags map[string]string
		Expect(json.Unmarshal(pipOutput, &pipTags)).To(Succeed())
		Expect(pipTags).To(HaveKeyWithValue(consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue))

		Expect(natGateway.Tags).To(HaveKeyWithValue(consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue),
			"a managed NAT Gateway must carry the CCM managed marker used by orphan cleanup")

		utils.Logf("  Service Gateway: %s", props.ServiceGateway.ID)
		Expect(props.ServiceGateway.ID).To(ContainSubstring(serviceGatewayName),
			"NAT Gateway should be associated with the Service Gateway")

		By("Verifying pod IPs registered in Address Locations")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, numPods)).To(Succeed())

		utils.Logf("\n✓ Outbound NAT Gateway test passed: %d pods", numPods)
	})

	It("should handle multiple egress gateways with different labels", func() {
		const (
			podsPerGateway = 8
			waitTime       = 90 * time.Second
			targetPort     = 8080
		)

		egressGateways := []string{"egress-alpha", "egress-beta-natgw", "egress-gamma-lb"}
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
				want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
				if err != nil {
					return err
				}
				if podCount != podsPerGateway {
					return fmt.Errorf("egress %s: expected %d active pods, got %d", egressName, podsPerGateway, podCount)
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
					if svc.Name == outboundServiceName(expectedGateway) {
						foundGateways[expectedGateway] = svc.Properties.PublicNatGatewayID
						utils.Logf("Found egress gateway '%s' with NAT Gateway: %s", expectedGateway, svc.Properties.PublicNatGatewayID)
						break
					}
				}
			}
		}

		Expect(len(foundGateways)).To(Equal(len(egressGateways)), fmt.Sprintf("Expected %d egress gateways, found %d", len(egressGateways), len(foundGateways)))

		By("Verifying pod IPs for each egress gateway")
		for _, gateway := range egressGateways {
			Expect(egressPodsRegisteredErr(cs, ns.Name, gateway, podsPerGateway)).To(Succeed())
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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != initialPods {
				return fmt.Errorf("expected %d active egress pods, got %d", initialPods, podCount)
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be registered with initial pod IPs")

		By("Verifying initial state")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, initialPods)).To(Succeed())

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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != finalPods {
				return fmt.Errorf("expected %d active egress pods, got %d", finalPods, podCount)
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be updated with scaled pod IPs")

		By("Verifying scaled state")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, finalPods)).To(Succeed())

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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != initialPods {
				return fmt.Errorf("expected %d active egress pods, got %d", initialPods, podCount)
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"egress service should be registered with initial pod IPs")

		By("Verifying initial state")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, initialPods)).To(Succeed())

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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != remainPods {
				return fmt.Errorf("expected %d surviving egress pods, got %d", remainPods, podCount)
			}
			return egressRegisteredMatchErr(egressName, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"exactly the surviving egress pods' IPs must remain registered after deletion")

		By("Verifying cleanup")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, remainPods)).To(Succeed())

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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != egressPods {
				return fmt.Errorf("egress %s: expected %d active pods, got %d", egressName, egressPods, podCount)
			}
			if err := egressRegisteredMatchErr(egressName, want); err != nil {
				return err
			}
			if err := servicePodsReconciledErr(cs, ns.Name, serviceUID, inboundPods); err != nil {
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
			if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
				foundOutbound = true
				utils.Logf("Found outbound service: %s", egressName)
			}
			if svc.Properties.ServiceType == "Inbound" && svc.Name == inboundServiceName(serviceUID) {
				foundInbound = true
				utils.Logf("Found inbound service: %s", serviceUID)
			}
		}

		Expect(foundOutbound).To(BeTrue(), "Outbound service should exist")
		Expect(foundInbound).To(BeTrue(), "Inbound service should exist")

		By("Verifying Address Locations for both services")
		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, egressPods)).To(Succeed())
		Expect(servicePodsReconciledErr(cs, ns.Name, serviceUID, inboundPods)).To(Succeed())

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
			want, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if podCount != dualPods {
				return fmt.Errorf("egress %s: expected %d active pods, got %d", egressName, dualPods, podCount)
			}
			if err := egressRegisteredMatchErr(egressName, want); err != nil {
				return err
			}
			if err := servicePodsReconciledErr(cs, ns.Name, serviceUID, dualPods); err != nil {
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
			if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
				foundOutbound = true
				utils.Logf("Found outbound service (NAT Gateway): %s", egressName)
			}
			if svc.Properties.ServiceType == "Inbound" && svc.Name == inboundServiceName(serviceUID) {
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
					if svcName == outboundServiceName(egressName) {
						egressPodIPs[addr.Address] = true
					}
					if svcName == inboundServiceName(serviceUID) {
						inboundPodIPs[addr.Address] = true
					}
				}
			}
		}

		utils.Logf("Pod IPs in outbound (NAT Gateway): %d", len(egressPodIPs))
		utils.Logf("Pod IPs in inbound (LB): %d", len(inboundPodIPs))

		// Counting registrations cannot distinguish a correct registration from a stale or foreign
		// address: two wrong pod IPs satisfy a count of two just as well as the two right ones.
		// The dual-traffic pods carry both labels. Egress uses every eligible family; inbound
		// uses only the families served by the Service.
		wantIPs, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
		Expect(err).NotTo(HaveOccurred())
		Expect(podCount).To(Equal(dualPods))
		wantInbound, inboundPodCount, err := livePodIPsForSelector(cs, ns.Name, createdService.Spec.Selector, createdService.Spec.IPFamilies)
		Expect(err).NotTo(HaveOccurred())
		Expect(inboundPodCount).To(Equal(dualPods))
		wantList := make([]string, 0, len(wantIPs))
		for ip := range wantIPs {
			wantList = append(wantList, ip)
		}
		wantInboundList := make([]string, 0, len(wantInbound))
		for ip := range wantInbound {
			wantInboundList = append(wantInboundList, ip)
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
		Expect(inboundList).To(ConsistOf(wantInboundList),
			"the inbound service must register exactly the eligible addresses of its served families")

		By("Verifying the SAME pod IPs are in both services")
		dualRegisteredCount := 0
		for ip := range egressPodIPs {
			if inboundPodIPs[ip] {
				dualRegisteredCount++
				utils.Logf("Pod IP %s is registered in BOTH inbound and outbound services", ip)
			}
		}

		Expect(dualRegisteredCount).To(Equal(len(wantInbound)),
			"every inbound address of a dual-traffic pod must also be registered for egress")

		By("Deleting half of the dual-traffic pods")
		podsToDelete := dualPods / 2
		for i := 0; i < podsToDelete; i++ {
			err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), fmt.Sprintf("dual-pod-%d", i), metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for deletion to propagate")
		expectedRemaining := dualPods - podsToDelete
		Eventually(func() error {
			wantEgress, podCount, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			wantInbound, inboundPodCount, err := livePodIPsForSelector(cs, ns.Name, createdService.Spec.Selector, createdService.Spec.IPFamilies)
			if err != nil {
				return err
			}
			if podCount != expectedRemaining || inboundPodCount != expectedRemaining {
				return fmt.Errorf("remaining pods: outbound=%d inbound=%d, want %d", podCount, inboundPodCount, expectedRemaining)
			}
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
						if svcName == outboundServiceName(egressName) {
							remainingEgress++
							remainingEgressIPs[addr.Address] = true
						}
						if svcName == inboundServiceName(serviceUID) {
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

			if remainingEgress != len(wantEgress) {
				return fmt.Errorf("got %d outbound addresses after deletion, want %d", remainingEgress, len(wantEgress))
			}
			if remainingInbound != len(wantInbound) {
				return fmt.Errorf("got %d inbound addresses after deletion, want %d", remainingInbound, len(wantInbound))
			}
			if remainingDual != len(wantInbound) {
				return fmt.Errorf("got %d addresses in both services after deletion, want %d", remainingDual, len(wantInbound))
			}
			if err := registeredAddressesMatchErr(outboundServiceName(egressName), wantEgress); err != nil {
				return err
			}
			return registeredAddressesMatchErr(inboundServiceName(serviceUID), wantInbound)
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
					if svcName == outboundServiceName(egressName) {
						remainingEgress++
						remainingEgressIPs[addr.Address] = true
					}
					if svcName == inboundServiceName(serviceUID) {
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

		Expect(egressPodsRegisteredErr(cs, ns.Name, egressName, expectedRemaining)).To(Succeed())
		Expect(servicePodsReconciledErr(cs, ns.Name, serviceUID, expectedRemaining)).To(Succeed())
		Expect(remainingDual).To(Equal(remainingInbound))

		utils.Logf("\n✓ Dual inbound+outbound pod test passed: %d pods with both LB and NAT Gateway", dualPods)
	})

	It("should require the same-named BYO NAT Gateway in the annotated resource group and only unlink it on deletion", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-egress-" + suffix + "-natgw"
		otherGateway := createBYONATGateway(byoRG, "other-"+suffix, byoAddresses{})
		By("Rejecting the missing same-named gateway rather than using another gateway or creating one")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, byoRG, fmt.Sprintf("NAT Gateway %q not found", egressName))
		Expect(outboundServiceGoneErr(egressName)).To(Succeed())
		Expect(byoNATGatewayLink(otherGateway)).To(BeEmpty())
		expectNoManagedNATGateway(egressName)
		missing, missingErr := runAz("rest", "--method", "get", "--url", armURL(byoNATGatewayARMID(byoRG, egressName)))
		Expect(missingErr).To(HaveOccurred(), "CCM must not create the missing BYO gateway")
		Expect(string(missing)).To(ContainSubstring("ResourceNotFound"))

		By("Creating the same-named gateway so the existing pods' retry can use it")
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)
		By("Waiting for the identity to be registered with the BYO NAT Gateway")
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "the BYO NAT Gateway must be linked to the Service Gateway")
		eventuallyNamespaceEvent(cs, ns.Name, "ServiceGatewayEgressNATGatewayLinked", time.Minute)
		out, err := runAz("network", "nat", "gateway", "show", "-g", resourceGroupName, "-n", outboundServiceName(egressName))
		Expect(err).To(HaveOccurred(), "no managed NAT Gateway may be created for an identity served by a BYO NAT Gateway")
		Expect(string(out)).To(ContainSubstring("ResourceNotFound"))

		By("Checking egress leaves through the BYO NAT Gateway's Public IP when the environment carries egress traffic")
		carriesEgress := expectEgressThroughNATGateway(cs, ns.Name, egressPodName(egressName, 0), natGatewayID)

		By("Deleting the egress pods and checking the BYO resources are only unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)

		By("Creating the pods again: the same BYO NAT Gateway must be linked again")
		// Wait for old finalizers before reusing pod names.
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, egressName) }, 2*time.Minute, defaultPollInterval).Should(BeZero())
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)

		if !carriesEgress {
			Skip("control plane verified; no egress could be observed on this cluster (no pod showed an outbound address), so SNAT through the BYO NAT Gateway was not checked")
		}
	})

	It("should link a BYO NAT Gateway without a Public IP, and egress through it once one is attached", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-fix-" + suffix

		By("Creating a BYO NAT Gateway without a Public IP")
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, byoAddresses{})

		By("Creating egress pods that name it: the identity is registered on it although it has no address")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		events, err := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, e := range events.Items {
			if e.Reason != "ServiceGatewayEgressNATGatewayRejected" {
				continue
			}
			if byoGrantedResourceGroups[byoRG] && strings.Contains(e.Message, "access denied") {
				continue
			}
			Fail("a NAT Gateway without addresses is its owner's to complete, not a reason to reject the identity: " + e.Message)
		}

		By("Attaching a Public IP, keeping the link: the pods egress through it without anything registered again")
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":%s,"publicIpAddressesV6":%s,"serviceGateway":{"id":%q}}}`,
			clusterLocation(), subResourceRefs(addresses.ipv4), subResourceRefs(addresses.ipv6), serviceGatewayResourceID()))
		Expect(byoRegisteredErr(cs, ns.Name, egressName, natGatewayID)).To(Succeed())
		carriesEgress := expectEgressThroughNATGateway(cs, ns.Name, egressPodName(egressName, 0), natGatewayID)

		By("Deleting the egress pod and checking the gateway is unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		if !carriesEgress {
			Skip("control plane verified; no egress could be observed on this cluster (no pod showed an outbound address), so SNAT through the attached Public IP was not checked")
		}
	})
	It("should reject a BYO NAT Gateway that is not StandardV2", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-sku-" + suffix

		By("Creating a Standard (not StandardV2) NAT Gateway")
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"Standard"}}`, clusterLocation()))

		By("Creating egress pods that name it: the identity must be rejected and nothing linked")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, byoRG, "SKU")
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "a rejected identity must not be registered")
		Expect(byoNATGatewayLink(natGatewayID)).To(BeEmpty(), "a rejected NAT Gateway must not be linked")
		expectNoManagedNATGateway(egressName)
	})

	It("should use a BYO NAT Gateway that has a Public IP prefix instead of a Public IP", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-prefix-" + suffix

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
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())))
		expectBYOPrefixesAttached(natGatewayID, prefixes)

		By("Deleting the pods: the gateway and prefix are only unlinked")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, prefixes)
		expectBYOPrefixesAttached(natGatewayID, prefixes)
	})

	It("should keep two BYO egress identities on their own NAT Gateways", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		first, second := "byo-one-"+suffix, "byo-two-"+suffix
		firstAddresses := createBYOAddresses(cs, byoRG, first)
		firstID := createBYONATGateway(byoRG, first, firstAddresses)
		secondAddresses := createBYOAddresses(cs, byoRG, second)
		secondID := createBYONATGateway(byoRG, second, secondAddresses)

		By("Creating pods for both identities")
		createBYOEgressPods(cs, ns.Name, first, byoRG, 1)
		createBYOEgressPods(cs, ns.Name, second, byoRG, 2)
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
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		byoName, managedName := "byo-mix-"+suffix, "managed-mix-"+suffix
		addresses := createBYOAddresses(cs, byoRG, byoName)
		natGatewayID := createBYONATGateway(byoRG, byoName, addresses)
		managedID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(managedName))

		By("Creating pods for both identities")
		createBYOEgressPods(cs, ns.Name, byoName, byoRG, 1)
		createEgressPods(cs, ns.Name, managedName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 3*time.Minute)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)
		out, err := runAz("rest", "--method", "get", "--url", armURL(managedID), "--query", "tags", "-o", "json")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(string(out)).To(ContainSubstring(fmt.Sprintf(`%q: %q`, consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue)))

		By("Scaling the BYO identity up to 3 pods and down to 1")
		createBYOEgressPodsFrom(cs, ns.Name, byoName, byoRG, 1, 2)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 2*time.Minute)
		for _, i := range []int{1, 2} {
			Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), egressPodName(byoName, i), metav1.DeleteOptions{})).To(Succeed())
		}
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, byoName) }, 3*time.Minute, defaultPollInterval).Should(Equal(1))
		eventuallyBYORegistered(cs, ns.Name, byoRG, byoName, natGatewayID, 2*time.Minute)

		By("Moving the last BYO pod to the managed identity: the BYO gateway is unlinked, the managed one gains the pod")
		relabelEgressPod(cs, ns.Name, egressPodName(byoName, 0), managedName, "")
		eventuallyBYOUnlinked(byoName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)
	})

	It("should keep inbound and BYO egress with the same raw identity independent", func() {
		service, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "ownership", Namespace: ns.Name},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP, Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		identity := string(service.UID)
		byoRG := byoResourceGroup(ns.Name[len(ns.Name)-5:])
		addresses := createBYOAddresses(cs, byoRG, identity)
		natID := createBYONATGateway(byoRG, identity, addresses)
		createBYOEgressPods(cs, ns.Name, identity, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, identity, natID, 3*time.Minute)

		By("Retyping the Service while BYO egress uses the same raw identity")
		Eventually(func() error {
			current, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), service.Name, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			current.Spec.Type = v1.ServiceTypeLoadBalancer
			current.Spec.Selector = map[string]string{egressLabel: identity}
			current.Spec.Ports[0].TargetPort = intstr.FromInt(8080)
			_, updateErr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), current, metav1.UpdateOptions{})
			return updateErr
		}, time.Minute, defaultPollInterval).Should(Succeed())
		inboundRegistered := func() error {
			want, podCount, listErr := livePodIPsForSelector(cs, ns.Name, map[string]string{egressLabel: identity}, service.Spec.IPFamilies)
			if listErr != nil {
				return listErr
			}
			if podCount != 1 {
				return fmt.Errorf("expected one Ready dual-traffic pod, got %d", podCount)
			}
			return serviceReconciledMatchErr(identity, want)
		}
		Eventually(func() error {
			if err := inboundRegistered(); err != nil {
				return err
			}
			return byoRegisteredErr(cs, ns.Name, identity, natID)
		}, 5*time.Minute, defaultPollInterval).Should(Succeed())
		service, err = utils.WaitServiceExposure(cs, ns.Name, service.Name, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(service.UID)).To(Equal(identity))
		pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), egressPodName(identity, 0), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Labels[egressLabel]).To(Equal(identity))
		services, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, registered := range services.Value {
			Expect(registered.Name).NotTo(Equal(identity), "the raw identity must not be registered")
		}

		By("Removing BYO egress without deleting the inbound resources")
		deleteEgressPods(cs, ns.Name, identity)
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, identity) }, 3*time.Minute, defaultPollInterval).Should(BeZero())
		eventuallyBYOUnlinked(identity, natID)
		eventuallyServiceReconciled(cs, ns.Name, identity, 0, 3*time.Minute)
		expectBYOResourcesExist(natID, addresses)

		By("Recreating BYO egress while inbound remains registered")
		createBYOEgressPods(cs, ns.Name, identity, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, identity, natID, 3*time.Minute)
		Eventually(inboundRegistered, 3*time.Minute, defaultPollInterval).Should(Succeed())

		By("Deleting inbound while BYO egress remains registered")
		Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), service.Name, metav1.DeleteOptions{})).To(Succeed())
		eventuallyServiceDeleted(identity, 5*time.Minute)
		Eventually(func() error {
			_, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), service.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				return nil
			}
			if getErr != nil {
				return getErr
			}
			return fmt.Errorf("inbound Service %s is still being deleted", service.Name)
		}, 5*time.Minute, defaultPollInterval).Should(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, identity, natID, 3*time.Minute)
		deleteEgressPods(cs, ns.Name, identity)
		eventuallyBYOUnlinked(identity, natID)
		expectBYOResourcesExist(natID, addresses)
	})

	It("should never take over the NAT Gateway of the default outbound service", func() {
		defaultServiceName, defaultNATGatewayID := defaultOutboundService()
		Expect(defaultNATGatewayID).NotTo(BeEmpty(), "the cluster must have a default outbound service")
		defaultName := strings.ToLower(resourceNameFromID(defaultNATGatewayID))
		defaultRG := resourceGroupFromID(defaultNATGatewayID)
		defaultLink := byoNATGatewayLink(defaultNATGatewayID)
		reservedIdentity := defaultName == "default-natgw" || outboundServiceName(defaultName) == "default-natgw" ||
			strings.EqualFold(outboundServiceName(defaultName), defaultServiceName)
		if len(validation.IsValidLabelValue(defaultName)) != 0 {
			Skip("the default gateway name cannot be expressed as an egress label")
		}
		By("Requesting the default gateway by its resource group and same-name identity: rejected")
		byoName := defaultName
		createBYOEgressPods(cs, ns.Name, byoName, defaultRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		if reservedIdentity {
			eventuallyNamespaceEvent(cs, ns.Name, "ServiceGatewayReservedEgressLabel", time.Minute)
		} else {
			eventuallyRejected(cs, ns.Name, defaultRG, fmt.Sprintf("Egress identity %q", outboundServiceName(byoName)))
			Expect(outboundServiceGoneErr(byoName)).To(Succeed(), "no additional identity may use the default gateway")
		}
		deleteEgressPods(cs, ns.Name, byoName)
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, byoName) }, 2*time.Minute, defaultPollInterval).Should(BeZero())

		By("Checking the default outbound service and its NAT Gateway are untouched")
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
		foreignID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(foreignName))
		DeferCleanup(deleteBYOResource, foreignID)
		putBYOResource(foreignID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"}}`, clusterLocation()))
		snapshot := natGatewayTagsAndLink(foreignID)
		Expect(snapshot).NotTo(ContainSubstring(consts.ServiceGatewayManagedTagKey))
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
		eventuallyRejected(cs, ns.Name, resourceGroupName, fmt.Sprintf("Egress identity %q", outboundServiceName(foreignName)))
		Expect(outboundServiceGoneErr(foreignName)).To(Succeed(), "a rejected identity must not be registered")
		deleteEgressPods(cs, ns.Name, foreignName)
		expectUnchanged()

		By("Rejecting explicit BYO selection of the cluster resource group")
		byoName := "byo-" + foreignName
		clearNamespaceEvents(cs, ns.Name)
		createBYOEgressPods(cs, ns.Name, byoName, resourceGroupName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, resourceGroupName, "is the cluster resource group")
		Expect(outboundServiceGoneErr(byoName)).To(Succeed(), "a rejected identity must not be registered")
		deleteEgressPods(cs, ns.Name, byoName)
		expectUnchanged()

		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; the start-up part of this spec needs a cloud-controller-manager restart")
		}
		By("Restarting the cloud-controller-manager, whose start-up cleanup must leave it alone")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())
		expectUnchanged()

		By("Creating egress pods named after it again: rejected by the live ownership check")
		clearNamespaceEvents(cs, ns.Name)
		createEgressPods(cs, ns.Name, foreignName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, resourceGroupName, fmt.Sprintf("Egress identity %q", outboundServiceName(foreignName)))
		deleteEgressPods(cs, ns.Name, foreignName)
		expectUnchanged()
		Eventually(func() (int, error) { return livePodCount(cs, ns.Name, foreignName) }, 2*time.Minute, defaultPollInterval).Should(BeZero())

		By("Using a same-named BYO gateway in another RG despite the unrelated node-RG gateway")
		byoRG := byoResourceGroup(ns.Name[len(ns.Name)-5:])
		addresses := createBYOAddresses(cs, byoRG, foreignName)
		externalID := createBYONATGateway(byoRG, foreignName, addresses)
		createBYOEgressPods(cs, ns.Name, foreignName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, foreignName, externalID, 3*time.Minute)
		deleteEgressPods(cs, ns.Name, foreignName)
		eventuallyBYOUnlinked(foreignName, externalID)
		expectUnchanged()
	})

	It("should keep treating a BYO NAT Gateway as BYO after the controller restarts", func() {
		if !IsCCMClusterConfigured() {
			Skip("CCM cluster access is not configured; cannot restart the cloud-controller-manager")
		}
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-restart-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Registering an identity on the BYO NAT Gateway")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 2)
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
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-lock-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Registering an identity on the BYO NAT Gateway")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Locking the BYO NAT Gateway read-only, then deleting the pod")
		lockID := natGatewayID + "/providers/Microsoft.Authorization/locks/byo-e2e-readonly"
		// Runs before the gateway's own cleanup: unlock, then clear the link the lock left behind.
		DeferCleanup(func() {
			_, _ = runAz("rest", "--method", "delete", "--url", "https://management.azure.com"+lockID+"?api-version=2016-09-01")
			if byoNATGatewayLink(natGatewayID) != "" {
				putBYONATGateway(byoRG, egressName, addresses)
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

	It("should reject reserved raw egress labels while allowing similar names", func() {
		defaultNATGatewayID := defaultOutboundNATGatewayID()
		Expect(defaultNATGatewayID).NotTo(BeEmpty(), "the cluster must have a default outbound service")

		for _, label := range []string{"default", "default-natgw"} {
			By(fmt.Sprintf("Rejecting the reserved raw label %q before registration or finalizers", label))
			createEgressPods(cs, ns.Name, label, 1)
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			Eventually(func() bool {
				return namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayReservedEgressLabel",
					fmt.Sprintf("Egress label %q is reserved", label))
			}, 2*time.Minute, defaultPollInterval).Should(BeTrue())
			pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), egressPodName(label, 0), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(hasFinalizer(pod.Finalizers, serviceGatewayPodFinalizer)).To(BeFalse())
			if label == "default-natgw" {
				Expect(egressOutboundGoneErr(label)).To(Succeed(), "a rejected label must not create an extra service or Azure resources")
			}
			deleteEgressPods(cs, ns.Name, label)
			Eventually(func() (int, error) { return livePodCount(cs, ns.Name, label) }, 2*time.Minute, defaultPollInterval).Should(BeZero())
		}

		By("Creating an ordinary egress identity with a similar name")
		const ordinary = "default-natgw2"
		createEgressPods(cs, ns.Name, ordinary, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		managedID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(ordinary))
		Expect(strings.EqualFold(managedID, defaultNATGatewayID)).To(BeFalse())
		eventuallyManagedRegistered(cs, ns.Name, ordinary, managedID)

		By("Deleting the managed egress pod without touching the default outbound service")
		deleteEgressPods(cs, ns.Name, ordinary)
		Eventually(func() error { return egressOutboundGoneErr(ordinary) }, 6*time.Minute, defaultPollInterval).Should(Succeed())
		Consistently(func() string { return defaultOutboundNATGatewayID() }, time.Minute, defaultPollInterval).
			Should(Equal(defaultNATGatewayID), "the default outbound service must keep its NAT Gateway")
	})

	It("should use a BYO NAT Gateway that its owner already linked to the Service Gateway", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-prelinked-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := byoNATGatewayARMID(byoRG, egressName)
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":%s,"publicIpAddressesV6":%s,"serviceGateway":{"id":%q}}}`,
			clusterLocation(), subResourceRefs(addresses.ipv4), subResourceRefs(addresses.ipv6), serviceGatewayResourceID()))
		Expect(strings.ToLower(byoNATGatewayLink(natGatewayID))).To(Equal(strings.ToLower(serviceGatewayResourceID())), "precondition: linked by its owner")

		By("Creating egress pods that name it")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)
		expectNoManagedNATGateway(egressName)

		By("Deleting the pods: the link is removed like any other BYO link")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
	})

	It("should reject a BYO NAT Gateway linked to another Service Gateway and leave that link alone", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-othersgw-" + suffix

		By("Creating another Service Gateway (with its own VNet) in the BYO resource group")
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
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
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

		By("Creating a NAT Gateway and Public IP with the CCM managed marker in the cluster resource group")
		pipID := createClusterPublicIP(PublicIPNameForEgress(egressName), true)
		natGatewayID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(egressName))
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":{%q:%q},"properties":{"publicIpAddresses":[{"id":%q}]}}`,
			clusterLocation(), consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue, pipID))

		By("Creating egress pods: the identity must use it as its managed NAT Gateway")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyManagedRegistered(cs, ns.Name, egressName, natGatewayID)

		By("Deleting the pods: the controller owns it and deletes it")
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyResourcesDeleted(natGatewayID, pipID)
	})

	It("should never adopt an untagged NAT Gateway with canonical managed resource names", func() {
		egressName := "managed-pretag-" + ns.Name[len(ns.Name)-5:]

		By("Creating an untagged NAT Gateway and Public IP with canonical managed names")
		pipID := createClusterPublicIP(PublicIPNameForEgress(egressName), false)
		natGatewayID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(egressName))
		DeferCleanup(deleteBYOResource, natGatewayID)
		putBYOResource(natGatewayID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":[{"id":%q}]}}`, clusterLocation(), pipID))
		snapshot := natGatewayTagsAndLink(natGatewayID)

		By("Creating egress pods named after it: rejected, and it is neither tagged, linked nor deleted")
		createEgressPods(cs, ns.Name, egressName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyRejected(cs, ns.Name, resourceGroupName, fmt.Sprintf("Egress identity %q", outboundServiceName(egressName)))
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "a rejected identity must not be registered")
		deleteEgressPods(cs, ns.Name, egressName)
		Consistently(func() (string, error) {
			if _, err := runAz("rest", "--method", "get", "--url", armURL(pipID)); err != nil {
				return "", err
			}
			return natGatewayTagsAndLink(natGatewayID), nil
		}, time.Minute, defaultPollInterval).Should(Equal(snapshot), "an untagged NAT Gateway and its Public IP must be left as they are")
	})

	It("should move pods between BYO and managed egress identities", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		first, second, managedName := "byo-move-a-"+suffix, "byo-move-b-"+suffix, "managed-move-"+suffix
		firstAddresses := createBYOAddresses(cs, byoRG, first)
		firstID := createBYONATGateway(byoRG, first, firstAddresses)
		secondAddresses := createBYOAddresses(cs, byoRG, second)
		secondID := createBYONATGateway(byoRG, second, secondAddresses)
		managedID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(managedName))

		createBYOEgressPods(cs, ns.Name, first, byoRG, 1)
		createBYOEgressPods(cs, ns.Name, second, byoRG, 1)
		createEgressPods(cs, ns.Name, managedName, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, first, firstID, 3*time.Minute)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, 3*time.Minute)
		eventuallyManagedRegistered(cs, ns.Name, managedName, managedID)

		By("Moving the first BYO identity's only pod to the second: the first gateway is unlinked")
		relabelEgressPod(cs, ns.Name, egressPodName(first, 0), second, byoRG)
		eventuallyBYOUnlinked(first, firstID)
		eventuallyBYORegistered(cs, ns.Name, byoRG, second, secondID, 3*time.Minute)

		By("Moving the managed identity's only pod to the first BYO identity, naming its NAT Gateway: the managed gateway is deleted, the BYO one linked again")
		relabelEgressPod(cs, ns.Name, egressPodName(managedName, 0), first, byoRG)
		eventuallyResourcesDeleted(managedID)
		eventuallyBYORegistered(cs, ns.Name, byoRG, first, firstID, 3*time.Minute)
		expectBYOResourcesExist(firstID, firstAddresses)
		expectBYOResourcesExist(secondID, secondAddresses)
	})

	It("should keep a BYO identity on its NAT Gateway when its last pod is replaced at once", func() {
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-replace-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 3*time.Minute)

		By("Deleting the only pod and creating a replacement straight away")
		Expect(cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), egressPodName(egressName, 0), metav1.DeleteOptions{})).To(Succeed())
		createBYOEgressPodsFrom(cs, ns.Name, egressName, byoRG, 1, 1)
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
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-down-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 2)
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
		tagged, preTag, foreign := "orphan-tagged-"+suffix, "orphan-pretag-"+suffix, "orphan-foreign-"+suffix

		By("Creating tagged, untagged and foreign unreferenced NAT Gateways in the cluster resource group")
		taggedPIP := createClusterPublicIP(PublicIPNameForEgress(tagged), true)
		taggedID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(tagged))
		DeferCleanup(deleteBYOResource, taggedID)
		putBYOResource(taggedID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":{%q:%q},"properties":{"publicIpAddresses":[{"id":%q}]}}`,
			clusterLocation(), consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue, taggedPIP))
		preTagPIP := createClusterPublicIP(PublicIPNameForEgress(preTag), false)
		preTagID := byoNATGatewayARMID(resourceGroupName, outboundServiceName(preTag))
		DeferCleanup(deleteBYOResource, preTagID)
		putBYOResource(preTagID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIpAddresses":[{"id":%q}]}}`, clusterLocation(), preTagPIP))
		preTagSnapshot := natGatewayTagsAndLink(preTagID)
		foreignID := byoNATGatewayARMID(resourceGroupName, foreign)
		DeferCleanup(deleteBYOResource, foreignID)
		putBYOResource(foreignID, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"}}`, clusterLocation()))
		foreignSnapshot := natGatewayTagsAndLink(foreignID)

		By("Restarting the cloud-controller-manager")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), GetCCMRecoveryTimeout())).To(Succeed())

		By("Its own tagged orphan is deleted; the untagged NAT Gateways are kept")
		eventuallyResourcesDeleted(taggedID, taggedPIP)
		Consistently(func() ([]string, error) {
			for _, id := range []string{foreignID, preTagID, preTagPIP} {
				if _, err := runAz("rest", "--method", "get", "--url", armURL(id)); err != nil {
					return nil, err
				}
			}
			return []string{natGatewayTagsAndLink(foreignID), natGatewayTagsAndLink(preTagID)}, nil
		}, 2*time.Minute, defaultPollInterval).Should(Equal([]string{foreignSnapshot, preTagSnapshot}),
			"an untagged NAT Gateway, including one with canonical managed names, must never be swept or changed")
	})

	It("should handle missing write access to BYO NAT Gateways without holding pods", func() {
		ids := byoAccessTestIdentities()
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		inUse := "byo-acc-used-" + suffix
		inUseAddresses := createBYOAddresses(cs, byoRG, inUse)
		inUseID := createBYONATGateway(byoRG, inUse, inUseAddresses)

		By("Registering an identity while the cluster identity has write access")
		createBYOEgressPods(cs, ns.Name, inUse, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		eventuallyBYORegistered(cs, ns.Name, byoRG, inUse, inUseID, 3*time.Minute)

		By("Reducing the cluster identity to Reader on the BYO resource group")
		restore := replaceClusterIdentityRoles(ids, byoRG, "Reader")
		DeferCleanup(func() {
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
			createBYOEgressPods(cs, ns.Name, probe, byoRG, 1)
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			denied := false
			Eventually(func() bool {
				denied = namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected",
					fmt.Sprintf("Egress identity %q cannot use NAT Gateway", outboundServiceName(name)), "access denied when linking")
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
		eventuallyBYORegistered(cs, ns.Name, byoRG, probe, probeID, 20*time.Minute)
		deleteEgressPods(cs, ns.Name, probe)
		eventuallyBYOUnlinked(probe, probeID)
	})

	It("should reject a BYO NAT Gateway it may not read, never fall back to a managed one, and use it once access is restored", func() {
		ids := byoAccessTestIdentities()
		suffix := ns.Name[len(ns.Name)-5:]
		byoRG := byoResourceGroup(suffix)
		egressName := "byo-noread-" + suffix
		addresses := createBYOAddresses(cs, byoRG, egressName)
		natGatewayID := createBYONATGateway(byoRG, egressName, addresses)

		By("Removing the cluster identity's access to the BYO resource group")
		restore := replaceClusterIdentityRoles(ids, byoRG, "")

		By("Probing with new identities that name a NAT Gateway that does not exist, until one is rejected because it may not be read")
		untilRoleChangeApplies(func(attempt int) bool {
			// A probe never links anything: its same-named gateway is absent.
			probe := fmt.Sprintf("byo-noread-probe-%s-%d", suffix, attempt)
			createBYOEgressPods(cs, ns.Name, probe, byoRG, 1)
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			rejected := fmt.Sprintf("Egress identity %q cannot be used", outboundServiceName(probe))
			denied := false
			Eventually(func() bool {
				denied = namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected", rejected, "access denied when reading")
				return denied || namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected", rejected, "not found in resource group")
			}, 3*time.Minute, defaultPollInterval).Should(BeTrue())
			deleteEgressPods(cs, ns.Name, probe)
			Eventually(func() (int, error) { return livePodCount(cs, ns.Name, probe) }, 6*time.Minute, defaultPollInterval).Should(BeZero(),
				"a rejected identity must not hold its pods")
			return denied
		})

		By("Creating the identity's pod while access is denied: rejected, no managed NAT Gateway, nothing linked")
		createBYOEgressPods(cs, ns.Name, egressName, byoRG, 1)
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
		Eventually(func() bool {
			return namespaceHasEventContaining(cs, ns.Name, "ServiceGatewayEgressNATGatewayRejected",
				fmt.Sprintf("Egress identity %q cannot be used", outboundServiceName(egressName)), "access denied when reading")
		}, 3*time.Minute, defaultPollInterval).Should(BeTrue())
		Expect(outboundServiceGoneErr(egressName)).To(Succeed(), "an identity whose NAT Gateway may not be read must not be registered")
		expectNoManagedNATGateway(egressName)
		Expect(byoNATGatewayLink(natGatewayID)).To(BeEmpty(), "a NAT Gateway that may not be read must not be linked")

		By("Restoring access: the identity is created on its BYO NAT Gateway without restarting its pod")
		restore()
		eventuallyBYORegistered(cs, ns.Name, byoRG, egressName, natGatewayID, 20*time.Minute)
		expectNoManagedNATGateway(egressName)
		deleteEgressPods(cs, ns.Name, egressName)
		eventuallyBYOUnlinked(egressName, natGatewayID)
		expectBYOResourcesExist(natGatewayID, addresses)
	})
})

// livePodIPsWithLabel returns the address set the pods carrying labelKey=labelValue should have
// registered. Egress specs compare against this set rather than a count: after a partial deletion
// only the set can distinguish the surviving pods from the deleted ones.
func livePodIPsWithLabel(cs clientset.Interface, namespace, labelKey, labelValue string) (map[string]struct{}, int, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{labelKey: labelValue}).String(),
	})
	if err != nil {
		return nil, 0, err
	}
	ready := make([]v1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil &&
			(pods.Items[i].Status.Phase == v1.PodRunning || pods.Items[i].Status.Phase == v1.PodPending) {
			ready = append(ready, pods.Items[i])
		}
	}
	addresses, err := egressPodIPSet(ready)
	if err == nil && len(ready) > 0 && len(addresses) == 0 {
		err = fmt.Errorf("no eligible egress addresses for %d active pods in namespace %s", len(ready), namespace)
	}
	return addresses, len(ready), err
}

// byoResourceAPIVersion supports StandardV2 NAT Gateways and Public IPs.
const byoResourceAPIVersion = "2025-05-01"

// byoGrantedResourceGroups tracks resource groups granted access by this spec,
// which may still be waiting for RBAC propagation.
var byoGrantedResourceGroups = map[string]bool{}

func byoResourceGroup(suffix string) string {
	rg := createEgressResourceGroup("e2e-sgw-byo-" + suffix)
	ids := clusterIdentityObjectIDs()
	grantEgressNetworkContributor(rg, ids)
	if len(ids) > 0 {
		byoGrantedResourceGroups[rg] = true
		DeferCleanup(func() { delete(byoGrantedResourceGroups, rg) })
	}
	return rg
}

// createEgressResourceGroup registers cleanup before creation, in reverse dependency order.
func createEgressResourceGroup(rg string) string {
	// An authorization refusal may also prevent checking or deleting the group.
	refused := false
	DeferCleanup(func() {
		if refused {
			return
		}
		if out, err := runAz("group", "exists", "--subscription", subscriptionID, "--name", rg); err == nil && strings.TrimSpace(string(out)) == "false" {
			return
		}
		out, err := runAz("group", "delete", "--subscription", subscriptionID, "--name", rg, "--yes")
		Expect(err).NotTo(HaveOccurred(), "deleting test resource group %s: %s", rg, string(out))
	})
	out, err := runAz("group", "create", "--subscription", subscriptionID, "--name", rg, "--location", clusterLocation(),
		"--tags", "purpose=cloud-provider-azure-e2e", "--output", "none")
	if err != nil && strings.Contains(string(out), "AuthorizationFailed") {
		refused = true
		Skip(fmt.Sprintf("the test identity cannot create resource group %s: %s", rg, string(out)))
	}
	Expect(err).NotTo(HaveOccurred(), string(out))
	return rg
}

func grantEgressNetworkContributor(rg string, ids []string) {
	scope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscriptionID, rg)
	for _, id := range ids {
		DeferCleanup(func() {
			_, _ = runAz("role", "assignment", "delete", "--assignee", id, "--role", "Network Contributor", "--scope", scope)
		})
		out, err := runAz("role", "assignment", "create", "--assignee-object-id", id, "--assignee-principal-type", "ServicePrincipal",
			"--role", "Network Contributor", "--scope", scope)
		Expect(err).NotTo(HaveOccurred(), string(out))
	}
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

func deleteBYOResource(resourceID string) {
	out, err := runAz("resource", "delete", "--ids", resourceID)
	Expect(err).NotTo(HaveOccurred(), "deleting %s: %s", resourceID, string(out))
}

func putBYOResource(resourceID, body string) {
	out, err := runAz("rest", "--method", "put", "--url", armURL(resourceID), "--body", body)
	Expect(err).NotTo(HaveOccurred(), string(out))
	Eventually(func() (string, error) {
		out, err := runAz("rest", "--method", "get", "--url", armURL(resourceID), "--query", "properties.provisioningState", "-o", "tsv")
		return strings.TrimSpace(string(out)), err
	}, 3*time.Minute, defaultPollInterval).Should(Equal("Succeeded"), "resource %s should be provisioned", resourceID)
}

type byoAddresses struct{ ipv4, ipv6 string }

func createBYOAddresses(cs clientset.Interface, rg, name string) byoAddresses {
	addresses := byoAddresses{ipv4: createBYOPublicIP(rg, name+"-pip", "IPv4")}
	if clusterHasIPv6Node(cs) {
		addresses.ipv6 = createBYOPublicIP(rg, name+"-pip-v6", "IPv6")
	}
	return addresses
}

func createBYOPublicIP(rg, name, version string) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", subscriptionID, rg, name)
	// Register cleanup before any write: a failed PUT may still create the resource.
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"publicIPAllocationMethod":"Static","publicIPAddressVersion":%q}}`, clusterLocation(), version))
	return id
}

// createBYONATGateway registers cleanup; call it after creating addresses so reverse-order
// cleanup removes the NAT Gateway first.
func createBYONATGateway(rg, name string, addresses byoAddresses) string {
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

func egressSourceIP(namespace, podName string) string {
	return egressSourceIPFrom(utils.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c", "curl -4 -s -m 10 ifconfig.me/ip || true"))
}

// egressSourceIPFrom extracts a complete IPv4 address from RunKubectl's wrapped stdout,
// never an address embedded in an exec error.
func egressSourceIPFrom(out string, err error) string {
	if err != nil {
		return ""
	}
	out, _, _ = strings.Cut(strings.TrimPrefix(out, "stdout:"), "\nstderr:")
	if ip := net.ParseIP(strings.TrimSpace(out)); ip != nil && ip.To4() != nil {
		return ip.String()
	}
	return ""
}

type egressOutcome int

const (
	egressUnobservable egressOutcome = iota
	egressThroughNATGateway
	egressElsewhere
)

func classifyEgress(natIPs, seen []string, controlSawEgress bool) egressOutcome {
	switch {
	case slices.ContainsFunc(seen, func(ip string) bool { return ip != "" && slices.Contains(natIPs, ip) }):
		return egressThroughNATGateway
	case controlSawEgress || slices.ContainsFunc(seen, func(ip string) bool { return ip != "" }):
		return egressElsewhere
	}
	return egressUnobservable
}

// expectEgressThroughNATGateway returns false only when both test and default-egress
// control pods are unobservable.
func expectEgressThroughNATGateway(cs clientset.Interface, namespace, podName, natGatewayID string) bool {
	natIPs, err := getNatGatewayPublicIPs(natGatewayID)
	Expect(err).NotTo(HaveOccurred())
	var seen []string
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(10 * time.Second) {
		seen = append(seen, egressSourceIP(namespace, podName))
		if classifyEgress(natIPs, seen, false) == egressThroughNATGateway {
			return true
		}
		if time.Now().After(deadline) {
			break
		}
	}
	controlSawEgress := false
	if classifyEgress(natIPs, seen, false) == egressUnobservable {
		control := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "egress-control", Namespace: namespace}, Spec: v1.PodSpec{Containers: []v1.Container{{
			Name: "test-app", Image: utils.AgnhostImage, ImagePullPolicy: v1.PullIfNotPresent, Args: []string{"netexec", "--http-port=8080"},
		}}}}
		_, err := cs.CoreV1().Pods(namespace).Create(context.TODO(), control, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, namespace)).To(Succeed())
		for i := 0; i < 3 && !controlSawEgress; i++ {
			controlSawEgress = egressSourceIP(namespace, control.Name) != ""
		}
	}
	if classifyEgress(natIPs, seen, controlSawEgress) == egressUnobservable {
		return false
	}
	Fail(fmt.Sprintf("pod %s must egress through NAT Gateway %s (%v); the source addresses seen were %q (empty: no egress)",
		podName, natGatewayID, natIPs, seen))
	return false
}

func byoNATGatewayLink(natGatewayID string) string {
	out, err := runAz("rest", "--method", "get", "--url", armURL(natGatewayID), "--query", "properties.serviceGateway.id", "-o", "tsv")
	Expect(err).NotTo(HaveOccurred(), string(out))
	return strings.TrimSpace(string(out))
}

func createEgressPods(cs clientset.Interface, namespace, egressName string, count int) {
	createEgressPodsFrom(cs, namespace, egressName, 0, count)
}

func createEgressPodsFrom(cs clientset.Interface, namespace, egressName string, first, count int) {
	createEgressPodsWith(cs, namespace, egressName, first, count, nil)
}

func createBYOEgressPods(cs clientset.Interface, namespace, egressName, resourceGroup string, count int) {
	createBYOEgressPodsFrom(cs, namespace, egressName, resourceGroup, 0, count)
}

func createBYOEgressPodsFrom(cs clientset.Interface, namespace, egressName, resourceGroup string, first, count int) {
	createEgressPodsWith(cs, namespace, egressName, first, count, map[string]string{byoNATGatewayResourceGroupAnnotation: resourceGroup})
}

func createEgressPodsWith(cs clientset.Interface, namespace, egressName string, first, count int, annotations map[string]string) {
	for i := first; i < first+count; i++ {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: egressPodName(egressName, i), Namespace: namespace, Labels: map[string]string{egressLabel: egressName},
				Annotations: annotations},
			Spec: v1.PodSpec{Containers: []v1.Container{{
				Name: "test-app", Image: utils.AgnhostImage, ImagePullPolicy: v1.PullIfNotPresent, Args: []string{"netexec", "--http-port=8080"},
			}}},
		}
		_, err := cs.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
}

func outboundServiceUsesNATGatewayErr(cs clientset.Interface, namespace, egressName, natGatewayID string) error {
	want, podCount, err := livePodIPsWithLabel(cs, namespace, egressLabel, egressName)
	if err != nil {
		return err
	}
	if podCount == 0 {
		return fmt.Errorf("egress %s has no active pods", egressName)
	}
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return err
	}
	for _, svc := range sgResponse.Value {
		if svc.Name == outboundServiceName(egressName) && svc.Properties.ServiceType == "Outbound" {
			if svc.Properties.PublicNatGatewayID == "" || !strings.EqualFold(svc.Properties.PublicNatGatewayID, natGatewayID) {
				return fmt.Errorf("egress %s uses NAT Gateway %q, want %q", egressName, svc.Properties.PublicNatGatewayID, natGatewayID)
			}
			return registeredAddressesMatchErr(outboundServiceName(egressName), want)
		}
	}
	return fmt.Errorf("egress %s is not registered", egressName)
}

func outboundServiceGoneErr(egressName string) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return err
	}
	for _, svc := range sgResponse.Value {
		if svc.Name == outboundServiceName(egressName) {
			return fmt.Errorf("egress %s is still registered", egressName)
		}
	}
	return nil
}

func eventuallyNamespaceEvent(cs clientset.Interface, namespace, reason string, timeout time.Duration) {
	Eventually(func() bool { return namespaceHasEventContaining(cs, namespace, reason) }, timeout, defaultPollInterval).
		Should(BeTrue(), "expected a %s event in namespace %s", reason, namespace)
}

// skipIfBYONATGatewayInaccessible skips missing access only when the spec did not grant it.
// Otherwise keep retrying to allow role-assignment propagation.
func skipIfBYONATGatewayInaccessible(cs clientset.Interface, namespace, byoRG string) {
	if byoGrantedResourceGroups[byoRG] {
		return
	}
	events, err := cs.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, e := range events.Items {
		if e.Reason == "ServiceGatewayEgressNATGatewayRejected" && strings.Contains(e.Message, "access denied") {
			Skip(fmt.Sprintf("the cluster identity may not use the NAT Gateways in resource group %s (%s); set %s to the object IDs "+
				"of its identities so that the spec grants them Network Contributor there", byoRG, e.Message, byoAccessIdentitiesEnvVar))
		}
	}
}

func eventuallyBYORegistered(cs clientset.Interface, namespace, byoRG, egressName, natGatewayID string, timeout time.Duration) {
	if byoGrantedResourceGroups[byoRG] {
		timeout = max(timeout, 15*time.Minute)
	}
	Eventually(func() error {
		skipIfBYONATGatewayInaccessible(cs, namespace, byoRG)
		return byoRegisteredErr(cs, namespace, egressName, natGatewayID)
	}, timeout, defaultPollInterval).Should(Succeed())
}

func byoRegisteredErr(cs clientset.Interface, namespace, egressName, natGatewayID string) error {
	if err := outboundServiceUsesNATGatewayErr(cs, namespace, egressName, natGatewayID); err != nil {
		return err
	}
	if link := byoNATGatewayLink(natGatewayID); !strings.EqualFold(link, serviceGatewayResourceID()) {
		return fmt.Errorf("BYO NAT Gateway is linked to %q, want %q", link, serviceGatewayResourceID())
	}
	return nil
}

func expectBYOResourcesExist(natGatewayID string, addresses byoAddresses) {
	for _, id := range []string{natGatewayID, addresses.ipv4, addresses.ipv6} {
		if id != "" {
			out, err := runAz("rest", "--method", "get", "--url", armURL(id))
			Expect(err).NotTo(HaveOccurred(), "BYO resource %s must survive the identity's deletion", id)
			var resource struct {
				Tags map[string]string `json:"tags"`
			}
			Expect(json.Unmarshal(out, &resource)).To(Succeed())
			Expect(resource.Tags).NotTo(HaveKey(consts.ServiceGatewayManagedTagKey), "BYO resources must not receive the CCM managed marker")
		}
	}
}

func expectBYOPrefixesAttached(natGatewayID string, prefixes byoAddresses) {
	for query, want := range map[string]string{"properties.publicIpPrefixes[].id": prefixes.ipv4, "properties.publicIpPrefixesV6[].id": prefixes.ipv6} {
		out, err := runAz("rest", "--method", "get", "--url", armURL(natGatewayID), "--query", query, "-o", "tsv")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(strings.ToLower(strings.TrimSpace(string(out)))).To(Equal(strings.ToLower(want)), "%s of NAT Gateway %s", query, natGatewayID)
	}
}

func defaultOutboundNATGatewayID() string {
	_, id := defaultOutboundService()
	return id
}

func defaultOutboundService() (name, natGatewayID string) {
	sgResponse, err := queryServiceGatewayServices()
	Expect(err).NotTo(HaveOccurred())
	for _, svc := range sgResponse.Value {
		if isDefaultOutboundService(svc) {
			return svc.Name, svc.Properties.PublicNatGatewayID
		}
	}
	return "", ""
}

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

func eventuallyManagedRegistered(cs clientset.Interface, namespace, egressName, natGatewayID string) {
	Eventually(func() error {
		return outboundServiceUsesNATGatewayErr(cs, namespace, egressName, natGatewayID)
	}, 3*time.Minute, defaultPollInterval).Should(Succeed(), "the managed identity must get a NAT Gateway in the cluster resource group")
}

func eventuallyRejected(cs clientset.Interface, namespace, rg, want string) {
	timeout := 2 * time.Minute
	if byoGrantedResourceGroups[rg] {
		// Allow RBAC propagation plus the controller's retry backoff.
		timeout = 15 * time.Minute
	}
	Eventually(func() bool {
		skipIfBYONATGatewayInaccessible(cs, namespace, rg)
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
	}, timeout, defaultPollInterval).Should(BeTrue(), "expected a ServiceGatewayEgressNATGatewayRejected Warning containing %q", want)
}

func clearNamespaceEvents(cs clientset.Interface, namespace string) {
	Expect(cs.CoreV1().Events(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
}

func expectNoManagedNATGateway(egressName string) {
	out, err := runAz("network", "nat", "gateway", "show", "-g", resourceGroupName, "-n", outboundServiceName(egressName))
	Expect(err).To(HaveOccurred(), "no managed NAT Gateway may be created for %s", egressName)
	Expect(string(out)).To(ContainSubstring("ResourceNotFound"))
}

func natGatewayTagsAndLink(natGatewayID string) string {
	out, err := runAz("rest", "--method", "get", "--url", armURL(natGatewayID), "--query", "{tags:tags,link:properties.serviceGateway.id}", "-o", "json")
	Expect(err).NotTo(HaveOccurred(), string(out))
	return string(out)
}

func resourceGroupFromID(id string) string {
	if match := resourceGroupInIDRegexp.FindStringSubmatch(id); match != nil {
		return match[1]
	}
	return ""
}

func subResourceRefs(id string) string {
	if id == "" {
		return "[]"
	}
	return fmt.Sprintf(`[{"id":%q}]`, id)
}

func createBYOPublicIPPrefix(rg, name, version string, length int) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPPrefixes/%s", subscriptionID, rg, name)
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"properties":{"prefixLength":%d,"publicIPAddressVersion":%q}}`,
		clusterLocation(), length, version))
	return id
}

// byoAccessIdentitiesEnvVar names the environment variable for cluster identity object IDs.
// Role changes are limited to the spec's resource groups and restored during cleanup.
const byoAccessIdentitiesEnvVar = "BYO_E2E_CLUSTER_IDENTITY_OBJECT_IDS"

func clusterIdentityObjectIDs() []string {
	var ids []string
	for _, id := range strings.Split(os.Getenv(byoAccessIdentitiesEnvVar), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func byoAccessTestIdentities() []string {
	ids := clusterIdentityObjectIDs()
	if len(ids) == 0 {
		Skip(byoAccessIdentitiesEnvVar + " is not set; this spec changes the cluster identity's role on the BYO resource group")
	}
	return ids
}

// replaceClusterIdentityRoles replaces scoped assignments and returns a restore function.
// Cleanup also restores the original assignments. Inherited grants would mask denial,
// so those cases must be skipped.
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
			case strings.HasPrefix(strings.ToLower(scope), strings.ToLower(a.Scope)+"/") && !strings.EqualFold(a.Role, role):
				Skip(fmt.Sprintf("identity %s has %q on %s, which also grants access to %s", id, a.Role, a.Scope, rg))
			}
		}
	}
	assign := func(roles map[string][]string) {
		for _, id := range ids {
			// Add replacement roles first; an empty target intentionally revokes access.
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

func relabelEgressPod(cs clientset.Interface, namespace, podName, egressName, resourceGroup string) {
	annotation := "null"
	if resourceGroup != "" {
		annotation = fmt.Sprintf("%q", resourceGroup)
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q},"annotations":{%q:%s}}}`, egressLabel, egressName, byoNATGatewayResourceGroupAnnotation, annotation)
	_, err := cs.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

// createClusterPublicIP creates a PIP fixture, optionally marked as CCM-managed.
func createClusterPublicIP(name string, managed bool) string {
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", subscriptionID, resourceGroupName, name)
	tags := "{}"
	if managed {
		tags = fmt.Sprintf(`{%q:%q}`, consts.ServiceGatewayManagedTagKey, consts.ServiceGatewayManagedTagValue)
	}
	DeferCleanup(deleteBYOResource, id)
	putBYOResource(id, fmt.Sprintf(`{"location":%q,"sku":{"name":"StandardV2"},"tags":%s,"properties":{"publicIPAllocationMethod":"Static","publicIPAddressVersion":"IPv4"}}`,
		clusterLocation(), tags))
	return id
}

func PublicIPNameForEgress(egressName string) string {
	return outboundServiceName(egressName) + "-pip"
}

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

// namespaceHasEventContaining matches the reason and all requested fragments.
// An empty fragment list matches the reason alone.
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

// untilRoleChangeApplies requires each attempt to use a new identity so a gateway linked
// before the role change cannot be mistaken for a successful post-change write.
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

func TestClassifyEgress(t *testing.T) {
	ips := []string{"134.138.232.22", "2001:db8::22"}
	for _, tc := range []struct {
		name             string
		natIPs           []string
		seen             []string
		controlSawEgress bool
		want             egressOutcome
	}{
		{name: "through a NAT Gateway address after probes that showed none", natIPs: ips, seen: []string{"", "134.138.232.22"}, want: egressThroughNATGateway},
		{name: "through its IPv6 address", natIPs: ips, seen: []string{"2001:db8::22"}, want: egressThroughNATGateway},
		{name: "through the NAT Gateway once the default egress stopped", natIPs: ips, seen: []string{"20.1.2.3", "134.138.232.22"}, want: egressThroughNATGateway},
		{name: "through another address only", natIPs: ips, seen: []string{"20.1.2.3", ""}, want: egressElsewhere},
		{name: "a NAT Gateway without Public IPs", seen: []string{"134.138.232.22"}, want: egressElsewhere},
		{name: "no egress from the pod while a control pod egresses", natIPs: ips, seen: []string{"", ""}, controlSawEgress: true, want: egressElsewhere},
		{name: "no egress anywhere", natIPs: ips, seen: []string{"", ""}, want: egressUnobservable},
		{name: "nothing probed", natIPs: ips, want: egressUnobservable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEgress(tc.natIPs, tc.seen, tc.controlSawEgress); got != tc.want {
				t.Errorf("classifyEgress(%v, %q, %v) = %d, want %d", tc.natIPs, tc.seen, tc.controlSawEgress, got, tc.want)
			}
		})
	}
}

func TestEgressSourceIPFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want string
	}{
		{name: "the address curl printed", out: "stdout:134.138.232.22\nstderr:", want: "134.138.232.22"},
		{name: "surrounding whitespace", out: "stdout: 134.138.232.22\n\nstderr:", want: "134.138.232.22"},
		{name: "no egress", out: "stdout:\nstderr:"},
		{name: "a failed exec naming the node's address", out: "stdout:\nstderr:error: dial tcp 10.224.0.5:10250: connect: connection refused",
			err: errors.New("exit status 1")},
		{name: "a failed exec that printed an address", out: "stdout:134.138.232.22\nstderr:error: stream closed", err: errors.New("exit status 1")},
		{name: "an address only in stderr", out: "stdout:\nstderr:10.224.0.5"},
		{name: "a page that is not an address", out: "stdout:<html>134.138.232.22</html>\nstderr:"},
		{name: "an IPv6 address", out: "stdout:2001:db8::1\nstderr:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressSourceIPFrom(tc.out, tc.err); got != tc.want {
				t.Errorf("egressSourceIPFrom(%q, %v) = %q, want %q", tc.out, tc.err, got, tc.want)
			}
		})
	}
}
