/*
Copyright 2022 The Kubernetes Authors.

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
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	cloudConfigNamespace = "kube-system"
	cloudConfigName      = "azure-cloud-provider"
	cloudConfigKey       = "cloud-config"
)

var _ = Describe("Cloud Config", Label(utils.TestSuiteLabelCloudConfig, utils.TestSuiteLabelSerial), func() {
	basename := "cloudconfig-service"
	serviceName := "cloudconfig-test"

	var (
		cs clientset.Interface
		ns *v1.Namespace
		tc *utils.AzureTestClient
	)

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		Port:       serverPort,
		TargetPort: intstr.FromInt(serverPort),
	}}

	BeforeEach(func() {
		if !strings.EqualFold(os.Getenv(utils.LoadCloudConfigFromSecret), "true") {
			Skip("Testing cloud config needs reading config from secret")
		}
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating Azure clients")
		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment %s", serviceName)
		deployment := createServerDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Waiting for backend pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support cloud config `Tags`, `SystemTags` and `TagsMap`", func() {
		By("Updating Tags and TagsMap in Cloudconfig")
		config, err := utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
		Expect(err).NotTo(HaveOccurred())
		// Make sure no other existing tags
		// Notice that the default tags for NSG will be cleaned
		config.SystemTags = "a, c, e, m, g=h"
		config.Tags = "a=b,c= d,e =, =f"
		config.TagsMap = map[string]string{"m": "n", "g=h": "i,j"}
		err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			// Notice that tags added in this test aren't cleaned currently
			By("Cleaning up Tags, SystemTags and TagsMap in cloud config secret")
			config, err := utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
			Expect(err).NotTo(HaveOccurred())
			config.Tags = ""
			config.SystemTags = ""
			config.TagsMap = map[string]string{}
			err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Creating a service")
		service := utils.CreateLoadBalancerServiceManifest(serviceName, nil, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting service to expose...")
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Cleaning up test service")
			err := utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		expectedTags := map[string]*string{
			"a":   to.StringPtr("b"),
			"c":   to.StringPtr("d"),
			"e":   to.StringPtr(""),
			"m":   to.StringPtr("n"),
			"g=h": to.StringPtr("i,j"),
		}

		By("Checking tags on the loadbalancer")
		err = waitCompareLBTags(tc, expectedTags, ip)
		Expect(err).NotTo(HaveOccurred())

		By("Checking tags of security group")
		err = waitCompareNsgTags(tc, expectedTags)
		Expect(err).NotTo(HaveOccurred())

		By("Checking tags of route tables")
		err = waitCompareRouteTableTags(tc, expectedTags)
		Expect(err).NotTo(HaveOccurred())

		By("Updating SystemTags in Cloudconfig")
		config, err = utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
		Expect(err).NotTo(HaveOccurred())
		config.SystemTags = "a"
		config.Tags = "u=w"
		config.TagsMap = map[string]string{}
		err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
		Expect(err).NotTo(HaveOccurred())

		expectedTags = map[string]*string{
			"a": to.StringPtr("b"),
			"u": to.StringPtr("w"),
		}

		By("Checking tags on the loadbalancer")
		err = waitCompareLBTags(tc, expectedTags, ip)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support cloud config `PrimaryScaleSetName` and `NodePoolsWithoutDedicatedSLB`", Label(utils.TestSuiteLabelMultiNodePools), func() {
		By("Get all the vmss names from all node's providerIDs")
		vmssNames, resourceGroupName, err := utils.GetAllVMSSNamesAndResourceGroup(cs)
		Expect(err).NotTo(HaveOccurred())

		// Skip if there're less than two vmss
		if len(vmssNames) < 2 {
			Skip("PrimaryScaleSetName and NodePoolsWithoutDedicatedSLB test only works for cluster with multiple vmss agent pools")
		}

		vmssList := vmssNames.List()[:2]

		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("NodePoolsWithoutDedicatedSLB test only works for standard lb")
		}

		By("Updating PrimaryScaleSetName in cloud config")
		config, err := utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
		Expect(err).NotTo(HaveOccurred())
		config.EnableMultipleStandardLoadBalancers = true
		config.NodePoolsWithoutDedicatedSLB = ""
		originalPrimaryScaleSetName := config.PrimaryScaleSetName
		config.PrimaryScaleSetName = vmssList[0]
		err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
		Expect(err).NotTo(HaveOccurred())

		service := utils.CreateLoadBalancerServiceManifest(serviceName, nil, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service %s in namespace %s", serviceName, ns.Name)

		// wait and get service's public IP Address
		By("Waiting for service exposure")
		publicIP, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Cleaning up test service")
			err := utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// validate load balancer backend pools for PrimaryScaleSetName
		backendPoolVMSSNames := getVMSSNamesInLoadBalancerBackendPools(tc, publicIP, tc.GetResourceGroup(), resourceGroupName)
		Expect(backendPoolVMSSNames.Equal(sets.NewString(vmssList[0]))).To(Equal(true))

		By("Updating NodePoolsWithoutDedicatedSLB in cloud config")
		config, err = utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
		Expect(err).NotTo(HaveOccurred())
		config.NodePoolsWithoutDedicatedSLB = strings.Join(vmssList, consts.VMSetNamesSharingPrimarySLBDelimiter)
		err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
		Expect(err).NotTo(HaveOccurred())

		// wait and validate load balancer backend pools for NodePoolsWithoutDedicatedSLB
		err = wait.PollImmediate(10*time.Second, 5*time.Minute, func() (done bool, err error) {
			backendPoolVMSSNames := getVMSSNamesInLoadBalancerBackendPools(tc, publicIP, tc.GetResourceGroup(), resourceGroupName)
			return backendPoolVMSSNames.Equal(sets.NewString(vmssList...)), nil
		})
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Cleaning up EnableMultipleStandardLoadBalancers in cloud config secret")
			config, err := utils.GetConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey)
			Expect(err).NotTo(HaveOccurred())
			config.EnableMultipleStandardLoadBalancers = false
			config.NodePoolsWithoutDedicatedSLB = ""
			config.PrimaryScaleSetName = originalPrimaryScaleSetName
			err = utils.UpdateConfigFromSecret(cs, cloudConfigNamespace, cloudConfigName, cloudConfigKey, config)
			Expect(err).NotTo(HaveOccurred())
		}()

	})
})

func waitCompareLBTags(tc *utils.AzureTestClient, expectedTags map[string]*string, ip string) error {
	err := wait.PollImmediate(10*time.Second, 2*time.Minute, func() (done bool, err error) {
		lb := getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "")
		return compareTags(lb.Tags, expectedTags), nil
	})
	return err
}

func waitCompareRouteTableTags(tc *utils.AzureTestClient, expectedTags map[string]*string) error {
	err := wait.PollImmediate(10*time.Second, 2*time.Minute, func() (done bool, err error) {
		routeTables, err := utils.ListRouteTables(tc)
		if err != nil {
			return false, err
		}

		rightTagFlag := true
		for _, routeTable := range *routeTables {
			utils.Logf("Checking tags for route table: %s", *routeTable.Name)
			if !compareTags(routeTable.Tags, expectedTags) {
				rightTagFlag = false
				break
			}
		}
		return rightTagFlag, nil
	})
	return err
}

func waitCompareNsgTags(tc *utils.AzureTestClient, expectedTags map[string]*string) error {
	err := wait.PollImmediate(10*time.Second, 2*time.Minute, func() (done bool, err error) {
		nsgs, err := tc.GetClusterSecurityGroups()
		if err != nil {
			return false, err
		}

		rightTagFlag := true
		for _, nsg := range nsgs {
			if !strings.Contains(*nsg.Name, "node") {
				continue
			}
			utils.Logf("Checking tags for nsg: %s", *nsg.Name)
			tags := nsg.Tags
			if !compareTags(tags, expectedTags) {
				rightTagFlag = false
				break
			}
		}
		return rightTagFlag, nil
	})
	return err
}
