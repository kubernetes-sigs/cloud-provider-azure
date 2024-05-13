/*
Copyright 2018 The Kubernetes Authors.

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
	"net/netip"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	aznetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Network security group", Label(utils.TestSuiteLabelNSG), func() {
	const (
		NamespaceSeed = "nsg"
		ServiceName   = "test-svc"
	)

	var (
		logger      = GinkgoLogr.WithName("NetworkSecurityGroup")
		k8sClient   clientset.Interface
		azureClient *utils.AzureTestClient
		namespace   *v1.Namespace
	)

	// Helpers
	var (
		derefSliceOfStringPtr = func(vs []*string) []string {
			rv := make([]string, 0, len(vs))
			for _, v := range vs {
				rv = append(rv, *v)
			}
			return rv
		}
		mustParseIPs = func(ips []string) []netip.Addr {
			rv := make([]netip.Addr, 0, len(ips))
			for _, ip := range ips {
				rv = append(rv, netip.MustParseAddr(ip))
			}
			return rv
		}
		groupIPsByFamily = func(ips []netip.Addr) (v4, v6 []netip.Addr) {
			for _, ip := range ips {
				if ip.Is4() {
					v4 = append(v4, ip)
				} else {
					v6 = append(v6, ip)
				}
			}
			return
		}
	)

	BeforeEach(func() {
		var err error
		k8sClient, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		namespace, err = utils.CreateTestingNamespace(NamespaceSeed, k8sClient)
		Expect(err).NotTo(HaveOccurred())

		azureClient, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		By("Applying the test deployment")
		deployment := createServerDeploymentManifest(ServiceName, map[string]string{
			"app": ServiceName,
		})
		_, err = k8sClient.AppsV1().Deployments(namespace.Name).Create(context.Background(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for backend pods to be ready")
		err = utils.WaitPodsToBeReady(k8sClient, namespace.Name)
		Expect(err).NotTo(HaveOccurred())
	})
	AfterEach(func() {
		if k8sClient != nil && namespace != nil {
			Expect(utils.DeleteNamespace(k8sClient, namespace.Name)).NotTo(HaveOccurred())
		}

		k8sClient, azureClient, namespace = nil, nil, nil
	})

	When("creating a default LoadBalancer service", func() {
		It("should add a rule to allow traffic from Internet", func() {
			var (
				serviceIPv4s []netip.Addr
				serviceIPv6s []netip.Addr
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{}
					ports       = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})

			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from Internet exists", func() {
				var (
					expectedProtocol    = aznetwork.SecurityRuleProtocolTCP
					expectedSrcPrefixes = []string{"Internet"}
					expectedDstPorts    []string
				)

				expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				if len(serviceIPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from Internet")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from Internet")
				}
			})
		})
	})

	When("creating an internal LoadBalancer service", func() {
		It("should not add any rules", func() {
			var (
				serviceIPv4s []netip.Addr
				serviceIPv6s []netip.Addr
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationLoadBalancerInternal: "true",
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from Internet exists", func() {
				if len(serviceIPv4s) > 0 {
					Expect(
						validator.NotHasRuleForDestination(serviceIPv4s),
					).To(BeTrue(), "Should not have rules for allowing IPv4 traffic from Internet")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.NotHasRuleForDestination(serviceIPv6s),
					).To(BeTrue(), "Should not have rules for allowing IPv6 traffic from Internet")
				}
			})
		})
	})

	When("creating a LoadBalancer service with `spec.LoadBalancerSourceRanges`", func() {
		It("should add a rule to allow traffic from allowed-IPs only", func() {
			var (
				serviceIPv4s      []netip.Addr
				serviceIPv6s      []netip.Addr
				allowedIPv4Ranges = []string{
					"10.20.0.0/16", "192.168.0.1/32",
				}
				allowedIPv6Ranges = []string{
					"2c0f:fe40:8000::/48", "2c0f:feb0::/43",
				}
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						v1.AnnotationLoadBalancerSourceRangesKey: strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ","),
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from allowed-IPs exists", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				)

				if len(serviceIPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv4Ranges, serviceIPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv4s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv4 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv4s),
					).To(BeFalse(), "Should not have a rule for denying all traffic")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv6Ranges, serviceIPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv6s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv6 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv6s),
					).To(BeFalse(), "Should not have a rule for denying all traffic")
				}
			})
		})
	})

	When("creating a LoadBalancer service with annotation `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges`", func() {
		It("should add a rule to allow traffic from allowed-IPs only", func() {
			var (
				serviceIPv4s      []netip.Addr
				serviceIPv6s      []netip.Addr
				allowedIPv4Ranges = []string{
					"10.20.0.0/16", "192.168.0.1/32",
				}
				allowedIPv6Ranges = []string{
					"2c0f:fe40:8000::/48", "2c0f:feb0::/43",
				}
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						v1.AnnotationLoadBalancerSourceRangesKey:                      strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ","),
						consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true",
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from allowed-IPs exists", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				)

				if len(serviceIPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv4Ranges, serviceIPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv4s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv4 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv4s),
					).To(BeTrue(), "Should not have a rule for denying all traffic")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv6Ranges, serviceIPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv6s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv6 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv6s),
					).To(BeTrue(), "Should have a rule for denying all traffic")
				}
			})
		})
	})

	When("creating a LoadBalancer service with annotation `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip`", func() {
		It("should add a rule to allow traffic from Internet", func() {
			var (
				serviceIPv4s []netip.Addr
				serviceIPv6s []netip.Addr
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from Internet exists", func() {
				Expect(
					validator.NotHasRuleForDestination(serviceIPv4s),
				).To(BeTrue(), "Should not have a rule for the LB IP address")
				Expect(
					validator.NotHasRuleForDestination(serviceIPv6s),
				).To(BeTrue(), "Should not have a rule for the LB IP address")
				// TODO: check backend pool IPs
			})
		})
	})

	When("creating a LoadBalancer service with annotation `service.beta.kubernetes.io/azure-additional-public-ips`", func() {
		It("should add a rule to allow traffic from Internet", func() {
			var (
				serviceIPv4s        []netip.Addr
				serviceIPv6s        []netip.Addr
				additionalPublicIPs = func() []string {
					var rv []string
					v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(azureClient.IPFamily)
					if v4Enabled {
						rv = append(rv, "10.20.0.1", "192.168.0.1")
					}
					if v6Enabled {
						rv = append(rv, "2c0f:fe40:8000::1", "2c0f:feb0::4")
					}
					return rv
				}()
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationAdditionalPublicIPs: strings.Join(additionalPublicIPs, ","),
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from Internet exists", func() {
				var (
					expectedProtocol                 = aznetwork.SecurityRuleProtocolTCP
					expectedSrcPrefixes              = []string{"Internet"}
					expectedDstPorts                 = []string{strconv.FormatInt(int64(serverPort), 10)}
					additionalIPv4s, additionalIPv6s = groupIPsByFamily(mustParseIPs(additionalPublicIPs))
				)
				if len(serviceIPv4s) > 0 {
					var expectedDstAddresses = append(serviceIPv4s, additionalIPv4s...)

					Expect(
						validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, expectedDstAddresses, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from Internet")
				}
				if len(serviceIPv6s) > 0 {
					var expectedDstAddresses = append(serviceIPv6s, additionalIPv6s...)

					Expect(
						validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, expectedDstAddresses, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from Internet")
				}
			})
		})
	})

	When("creating a LoadBalancer service with annotation `service.beta.kubernetes.io/azure-allowed-ip-ranges`", func() {
		It("should add a rule to allow traffic from allowed-IPs only", func() {
			var (
				serviceIPv4s      []netip.Addr
				serviceIPv6s      []netip.Addr
				allowedIPv4Ranges = []string{
					"10.20.0.0/16", "192.168.0.1/32",
				}
				allowedIPv6Ranges = []string{
					"2c0f:fe40:8000::/48", "2c0f:feb0::/43",
				}
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationAllowedIPRanges: strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ","),
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from allowed-IPs exists", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				)

				if len(serviceIPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv4Ranges, serviceIPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv4s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv4 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv4s),
					).To(BeFalse(), "Should not have a rule for denying all traffic")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv6Ranges, serviceIPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from allowed-IPs")
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv6s, expectedDstPorts),
					).To(BeFalse(), "Should not have a rule for allowing IPv6 traffic from Internet")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv6s),
					).To(BeFalse(), "Should not have a rule for denying all traffic")
				}
			})
		})
	})

	When("creating a LoadBalancer service with annotation `service.beta.kubernetes.io/azure-allowed-service-tags`", func() {
		It("should add a rule to allow traffic from allowed-service-tags only", func() {
			var (
				serviceIPv4s       []netip.Addr
				serviceIPv6s       []netip.Addr
				allowedServiceTags = []string{
					"AzureCloud",
					"AzureDatabricks",
				}
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationAllowedServiceTags: strings.Join(allowedServiceTags, ","),
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from allowed-service-tags exists", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				)

				for _, allowedServiceTag := range allowedServiceTags {
					By(fmt.Sprintf("Checking if the rule for allowing traffic from service tag %q exists", allowedServiceTag))
					var expectedSrcPrefixes = []string{allowedServiceTag}

					if len(serviceIPv4s) > 0 {
						Expect(
							validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv4s, expectedDstPorts),
						).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from service tag %q", allowedServiceTag)
						Expect(
							validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv4s, expectedDstPorts),
						).To(BeFalse(), "Should not have a rule for allowing IPv4 traffic from Internet")
						Expect(
							validator.HasDenyAllRuleForDestination(serviceIPv4s),
						).To(BeFalse(), "Should not have a rule for denying all traffic")
					}

					if len(serviceIPv6s) > 0 {
						Expect(
							validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv6s, expectedDstPorts),
						).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from service tag %q", allowedServiceTag)
						Expect(
							validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv6s, expectedDstPorts),
						).To(BeFalse(), "Should not have a rule for allowing IPv6 traffic from Internet")
						Expect(
							validator.HasDenyAllRuleForDestination(serviceIPv6s),
						).To(BeFalse(), "Should not have a rule for denying all traffic")
					}
				}

			})
		})
	})

	When("creating a LoadBalancer service with combination of annotations", func() {
		It("should add multiple rules to allow traffic from allowed-service-tags and allowed-IPs only", func() {
			var (
				serviceIPv4s      []netip.Addr
				serviceIPv6s      []netip.Addr
				allowedIPv4Ranges = []string{
					"10.20.0.0/16", "192.168.0.1/32",
				}
				allowedIPv6Ranges = []string{
					"2c0f:fe40:8000::/48", "2c0f:feb0::/43",
				}
				allowedServiceTags = []string{
					"AzureCloud",
					"AzureDatabricks",
				}
			)

			By("Creating a LoadBalancer service", func() {
				var (
					labels = map[string]string{
						"app": ServiceName,
					}
					annotations = map[string]string{
						consts.ServiceAnnotationAllowedServiceTags:                    strings.Join(allowedServiceTags, ","),
						consts.ServiceAnnotationAllowedIPRanges:                       strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ","),
						consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true",
					}
					ports = []v1.ServicePort{{
						Port:       serverPort,
						TargetPort: intstr.FromInt32(serverPort),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, ServiceName, namespace.Name, labels, annotations, ports)
				serviceIPv4s, serviceIPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
			})
			logger.Info("Created a LoadBalancer service", "v4-IPs", serviceIPv4s, "v6-IPs", serviceIPv6s)

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic from allowed-service-tags exists", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(serverPort), 10)}
				)

				for _, allowedServiceTag := range allowedServiceTags {
					By(fmt.Sprintf("Checking if the rule for allowing traffic from service tag %q exists", allowedServiceTag))
					var expectedSrcPrefixes = []string{allowedServiceTag}

					if len(serviceIPv4s) > 0 {
						Expect(
							validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv4s, expectedDstPorts),
						).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from service tag %q", allowedServiceTag)
						Expect(
							validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv4s, expectedDstPorts),
						).To(BeFalse(), "Should not have a rule for allowing IPv4 traffic from Internet")
					}

					if len(serviceIPv6s) > 0 {
						Expect(
							validator.HasExactAllowRule(expectedProtocol, expectedSrcPrefixes, serviceIPv6s, expectedDstPorts),
						).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from service tag %q", allowedServiceTag)
						Expect(
							validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, serviceIPv6s, expectedDstPorts),
						).To(BeFalse(), "Should not have a rule for allowing IPv6 traffic from Internet")
					}
				}

				if len(serviceIPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv4Ranges, serviceIPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from allowed-IPs")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv4s),
					).To(BeTrue(), "Should not have a rule for denying all traffic")
				}
				if len(serviceIPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, allowedIPv6Ranges, serviceIPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from allowed-IPs")
					Expect(
						validator.HasDenyAllRuleForDestination(serviceIPv6s),
					).To(BeTrue(), "Should not have a rule for denying all traffic")
				}

			})
		})
	})

	When("creating 2 LoadBalancer services with shared public IP", func() {
		It("should add rules independently", func() {

			const (
				Deployment1Name = "app-01"
				Deployment2Name = "app-02"

				Service1Name = "svc-01"
				Service2Name = "svc-02"
			)

			var (
				app1Port  int32 = 80
				app2Port  int32 = 81
				replicas  int32 = 2
				svc1IPv4s []netip.Addr
				svc1IPv6s []netip.Addr
				svc2IPs   []netip.Addr
			)

			deployment1 := createDeploymentManifest(Deployment1Name, map[string]string{
				"app": Deployment1Name,
			}, &app1Port, nil)
			deployment1.Spec.Replicas = &replicas
			_, err := k8sClient.AppsV1().Deployments(namespace.Name).Create(context.Background(), deployment1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			deployment2 := createDeploymentManifest(Deployment2Name, map[string]string{
				"app": Deployment2Name,
			}, &app2Port, nil)
			deployment2.Spec.Replicas = &replicas
			_, err = k8sClient.AppsV1().Deployments(namespace.Name).Create(context.Background(), deployment2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Creating service 1", func() {
				var (
					labels = map[string]string{
						"app": Deployment1Name,
					}
					annotations = map[string]string{}
					ports       = []v1.ServicePort{{
						Port:       app1Port,
						TargetPort: intstr.FromInt32(app1Port),
					}}
				)
				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, Service1Name, namespace.Name, labels, annotations, ports)
				svc1IPv4s, svc1IPv6s = groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
				logger.Info("Created the first LoadBalancer service", "svc-name", Service1Name, "v4-IPs", svc1IPv4s, "v6-IPs", svc1IPv6s)
			})

			By("Creating service 2", func() {

				joinIPsAsString := func(ips []netip.Addr) string {
					var s []string
					for _, ip := range ips {
						s = append(s, ip.String())
					}
					return strings.Join(s, ",")
				}

				var (
					labels = map[string]string{
						"app": Deployment2Name,
					}
					annotations = map[string]string{
						"service.beta.kubernetes.io/azure-load-balancer-ipv4": joinIPsAsString(svc1IPv4s),
						"service.beta.kubernetes.io/azure-load-balancer-ipv6": joinIPsAsString(svc1IPv6s),
					}
					ports = []v1.ServicePort{{
						Port:       app2Port,
						TargetPort: intstr.FromInt32(app2Port),
					}}
				)

				rv := createAndExposeDefaultServiceWithAnnotation(k8sClient, azureClient.IPFamily, Service2Name, namespace.Name, labels, annotations, ports)
				svc2IPv4s, svc2IPv6s := groupIPsByFamily(mustParseIPs(derefSliceOfStringPtr(rv)))
				logger.Info("Created the second LoadBalancer service", "svc-name", Service2Name, "v4-IPs", svc2IPv4s, "v6-IPs", svc2IPv6s)
				Expect(svc2IPv4s).To(Equal(svc1IPv4s))
				Expect(svc2IPv6s).To(Equal(svc1IPv6s))
			})

			var validator *SecurityGroupValidator
			By("Getting the cluster security groups", func() {
				rv, err := azureClient.GetClusterSecurityGroups()
				Expect(err).NotTo(HaveOccurred())

				validator = NewSecurityGroupValidator(rv)
			})

			By("Checking if the rule for allowing traffic for app 01", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(app1Port), 10)}
				)

				By("Checking if the rule for allowing traffic from Internet exists")

				if len(svc1IPv4s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, svc1IPv4s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv4 traffic from Internet")
				}

				if len(svc1IPv6s) > 0 {
					Expect(
						validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, svc1IPv6s, expectedDstPorts),
					).To(BeTrue(), "Should have a rule for allowing IPv6 traffic from Internet")
				}
			})

			By("Checking if the rule for allowing traffic for app 02", func() {
				var (
					expectedProtocol = aznetwork.SecurityRuleProtocolTCP
					expectedDstPorts = []string{strconv.FormatInt(int64(app2Port), 10)}
				)
				By("Checking if the rule for allowing traffic from Internet exists")
				Expect(
					validator.HasExactAllowRule(expectedProtocol, []string{"Internet"}, svc2IPs, expectedDstPorts),
				).To(BeTrue(), "Should have a rule for allowing traffic from Internet")
			})
		})
	})
})

type SecurityGroupValidator struct {
	nsgs []*aznetwork.SecurityGroup
}

func NewSecurityGroupValidator(nsgs []*aznetwork.SecurityGroup) *SecurityGroupValidator {
	// FIXME: should get the exact Security Group by virtual network subnets instead of listing all
	return &SecurityGroupValidator{
		nsgs: nsgs,
	}
}

// HasExactAllowRule checks if the security group has a rule that allows traffic from the given source prefixes to the given destination addresses and ports.
func (v *SecurityGroupValidator) HasExactAllowRule(
	protocol aznetwork.SecurityRuleProtocol,
	srcPrefixes []string,
	dstAddresses []netip.Addr,
	dstPorts []string,
) bool {
	for i := range v.nsgs {
		if SecurityGroupHasAllowRuleForDestination(v.nsgs[i], protocol, srcPrefixes, dstAddresses, dstPorts) {
			return true
		}
	}
	return false
}

// NotHasRuleForDestination checks if the security group has a rule specifying the given destination addresses.
func (v *SecurityGroupValidator) NotHasRuleForDestination(dstAddresses []netip.Addr) bool {
	for i := range v.nsgs {
		if !SecurityGroupNotHasRuleForDestination(v.nsgs[i], dstAddresses) {
			return false
		}
	}
	return true
}

// HasDenyAllRuleForDestination checks if the security group has a rule that denies all traffic to the given destination addresses.
func (v *SecurityGroupValidator) HasDenyAllRuleForDestination(dstAddresses []netip.Addr) bool {
	for i := range v.nsgs {
		if SecurityGroupHasDenyAllRuleForDestination(v.nsgs[i], dstAddresses) {
			return true
		}
	}
	return false
}

func SecurityGroupNotHasRuleForDestination(nsg *aznetwork.SecurityGroup, dstAddresses []netip.Addr) bool {
	logger := GinkgoLogr.WithName("SecurityGroupNotHasRuleForDestination").
		WithValues("nsg-name", nsg.Name).
		WithValues("dst-addresses", dstAddresses)
	if len(dstAddresses) == 0 {
		logger.Info("skip")
		return true
	}
	logger.Info("checking")
	dsts := sets.NewString()
	for _, ip := range dstAddresses {
		dsts.Insert(ip.String())
	}
	for _, rule := range nsg.Properties.SecurityRules {
		logger.Info("checking rule", "rule-name", rule.Name, "rule", rule)
		if rule.Properties.DestinationAddressPrefix != nil && dsts.Has(*rule.Properties.DestinationAddressPrefix) {
			return false
		}

		if rule.Properties.DestinationAddressPrefixes == nil {
			continue
		}
		for _, d := range rule.Properties.DestinationAddressPrefixes {
			if dsts.Has(*d) {
				return false
			}
		}
	}
	return true
}

func SecurityGroupHasAllowRuleForDestination(
	nsg *aznetwork.SecurityGroup,
	protocol aznetwork.SecurityRuleProtocol,
	srcPrefixes []string,
	dstAddresses []netip.Addr, dstPorts []string,
) bool {
	logger := GinkgoLogr.WithName("HasAllowRuleForDestination").
		WithValues("nsg-name", nsg.Name).
		WithValues("protocol", protocol).
		WithValues("src-prefixes", srcPrefixes).
		WithValues("dst-addresses", dstAddresses)
	if len(dstAddresses) == 0 {
		logger.Info("skip")
		return true
	}
	logger.Info("checking")

	var (
		expectedSrcPrefixes  = sets.NewString(srcPrefixes...)
		expectedDstPorts     = sets.NewString(dstPorts...)
		expectedDstAddresses = sets.NewString()
	)
	for _, ip := range dstAddresses {
		expectedDstAddresses.Insert(ip.String())
	}

	for _, rule := range nsg.Properties.SecurityRules {
		if *rule.Properties.Access != aznetwork.SecurityRuleAccessAllow ||
			*rule.Properties.Direction != aznetwork.SecurityRuleDirectionInbound ||
			*rule.Properties.Protocol != protocol ||
			ptr.Deref(rule.Properties.SourcePortRange, "") != "*" ||
			len(rule.Properties.DestinationAddressPrefixes) < len(expectedDstAddresses) ||
			len(rule.Properties.DestinationPortRanges) != len(dstPorts) {
			logger.Info("skip rule", "rule-name", rule.Name, "rule", rule)
			continue
		}
		logger.Info("checking rule", "rule-name", rule.Name, "rule", rule)

		{
			// check destination ports
			actualDstPorts := sets.NewString()
			for _, d := range rule.Properties.DestinationPortRanges {
				actualDstPorts.Insert(*d)
			}
			if !actualDstPorts.Equal(expectedDstPorts) {
				continue
			}
		}

		{
			// check source prefixes
			actualSrcPrefixes := sets.NewString()
			if rule.Properties.SourceAddressPrefix != nil {
				actualSrcPrefixes.Insert(*rule.Properties.SourceAddressPrefix)
			}
			for _, d := range rule.Properties.SourceAddressPrefixes {
				actualSrcPrefixes.Insert(*d)
			}
			if !actualSrcPrefixes.Equal(expectedSrcPrefixes) {
				continue
			}
		}

		// check destination addresses
		for _, d := range rule.Properties.DestinationAddressPrefixes {
			expectedDstAddresses.Delete(*d)
			if expectedDstAddresses.Len() == 0 {
				break
			}
		}

		if expectedDstAddresses.Len() == 0 {
			break
		}
	}

	if expectedDstAddresses.Len() > 0 {
		logger.Info("no rule for destination addresses", "addresses", expectedDstAddresses.List())
		return false
	}

	return true
}

func SecurityGroupHasDenyAllRuleForDestination(nsg *aznetwork.SecurityGroup, dstAddresses []netip.Addr) bool {
	logger := GinkgoLogr.WithName("HasDenyAllRuleForDestination").
		WithValues("nsg-name", nsg.Name).
		WithValues("dst-addresses", dstAddresses)
	if len(dstAddresses) == 0 {
		logger.Info("skip checking")
		return true
	}

	expectedDstAddresses := sets.NewString()
	for _, ip := range dstAddresses {
		expectedDstAddresses.Insert(ip.String())
	}

	for _, rule := range nsg.Properties.SecurityRules {
		if *rule.Properties.Access != aznetwork.SecurityRuleAccessDeny ||
			ptr.Deref(rule.Properties.SourceAddressPrefix, "") != "*" ||
			ptr.Deref(rule.Properties.SourcePortRange, "") != "*" ||
			ptr.Deref(rule.Properties.DestinationPortRange, "") != "*" {
			logger.Info("skip rule", "rule-name", rule.Name)
			continue
		}
		logger.Info("checking rule", "rule-name", rule.Name, "rule", rule)

		for _, d := range rule.Properties.DestinationAddressPrefixes {
			expectedDstAddresses.Delete(*d)
			if expectedDstAddresses.Len() == 0 {
				break
			}
		}

		if expectedDstAddresses.Len() == 0 {
			break
		}
	}

	if expectedDstAddresses.Len() > 0 {
		logger.Info("no rule for destination addresses", "addresses", expectedDstAddresses.List())
		return false
	}

	return true
}
