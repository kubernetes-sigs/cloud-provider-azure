/*
Copyright 2023 The Kubernetes Authors.

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
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Ensure LoadBalancer", Label(utils.TestSuiteLabelMultiSLB), func() {
	basename := testBaseName

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient
	var deployment *appsv1.Deployment

	labels := map[string]string{
		"app": testServiceName,
	}
	ports := []v1.ServicePort{{
		Port:       serverPort,
		TargetPort: intstr.FromInt(serverPort),
	}}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment %s", testDeploymentName)
		deployment = createServerDeploymentManifest(testDeploymentName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), testDeploymentName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
		}

		cs = nil
		ns = nil
		tc = nil
	})

	It("should send nodes to correct load balancers by primary vmSet", func() {
		if os.Getenv(utils.AKSTestCCM) != "" {
			Skip("This test is not supported for AKS CCM.")
		}

		var svcIPs []string
		svcCount := 2
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		clusterName := os.Getenv("CLUSTER_NAME")
		lbNameToExpectedVMSetNameMap := map[string]string{
			clusterName: fmt.Sprintf("%s-vmss-0", clusterName),
			"lb1":       fmt.Sprintf("%s-vmss-1", clusterName),
			"lb2":       fmt.Sprintf("%s-vmss-2", clusterName),
		}

		for _, svcIP := range svcIPs {
			lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
			lbName := pointer.StringDeref(lb.Name, "")
			for _, bp := range *lb.BackendAddressPools {
				for _, a := range *bp.LoadBalancerBackendAddresses {
					nodeName := pointer.StringDeref(a.Name, "")
					expectedVMSSName := lbNameToExpectedVMSetNameMap[lbName]
					if nodeName == "" || !strings.HasPrefix(nodeName, expectedVMSSName) {
						Fail(fmt.Sprintf("Node %s is not in the expected VMSS %s on LB %s", nodeName, expectedVMSSName, lbName))
					}
				}
			}
		}
	})

	It("should arrange services across load balancers correctly", func() {
		var svcIPs []string
		svcCount := 2
		var svc *v1.Service
		By(fmt.Sprintf("Creating %d services", svcCount))
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc = utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		By("Checking the load balancer count to equal 2")
		lbNames := getLBsFromPublicIPs(tc, svcIPs)
		Expect(len(lbNames)).To(Equal(2))

		By("Creating a service targeting to a new load balancer")
		l := map[string]string{
			"app": testServiceName,
			"a":   "b",
		}
		svcWithLabel := utils.CreateLoadBalancerServiceManifest("svc-with-label", nil, l, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svcWithLabel, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svsWithLabel, err := utils.WaitServiceExposure(cs, ns.Name, "svc-with-label", []string{})
		Expect(err).NotTo(HaveOccurred())
		svcIP := svsWithLabel.Status.LoadBalancer.Ingress[0].IP
		svcIPs = append(svcIPs, svcIP)
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		Expect(pointer.StringDeref(lb.Name, "")).To(Equal("lb-2"))

		By("Checking the load balancer count to equal 3")
		interval := 5 * time.Second
		timeout := 5 * time.Minute
		_, err = waitLBCountEqualTo(tc, interval, timeout, 3, svcIPs)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Updating service %s to move to another load balancer", svc.Name))
		updateServiceAnnotation(svc, map[string]string{"service.beta.kubernetes.io/azure-load-balancer-configurations": "lb-2"})
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the load balancer count to equal 2")
		lbNames, err = waitLBCountEqualTo(tc, interval, timeout, 2, svcIPs)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbNames.Has("lb-2")).To(BeTrue())
		Expect(lbNames.Has("lb-1")).To(BeFalse())
	})
})

func getLBsFromPublicIPs(tc *utils.AzureTestClient, pips []string) sets.Set[string] {
	lbNames := sets.New[string]()
	for _, svcIP := range pips {
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		lbName := pointer.StringDeref(lb.Name, "")
		lbNames.Insert(lbName)
	}
	return lbNames
}

func waitLBCountEqualTo(tc *utils.AzureTestClient, interval, timeout time.Duration, expectedCount int, svcIPs []string) (sets.Set[string], error) {
	var lbNames sets.Set[string]
	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		lbNames = getLBsFromPublicIPs(tc, svcIPs)
		if len(lbNames) != expectedCount {
			utils.Logf("Waiting for %d load balancers, found %d", expectedCount, len(lbNames))
			return false, nil
		}
		return true, nil
	})
	return lbNames, err
}
