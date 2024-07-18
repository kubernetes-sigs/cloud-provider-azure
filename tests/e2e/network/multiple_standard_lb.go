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
	"k8s.io/utils/ptr"

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

		var svcIPs []*string
		svcCount := 2
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		clusterName := os.Getenv("CLUSTER_NAME")
		lbNameToExpectedVMSetNameMap := map[string]string{
			clusterName: fmt.Sprintf("%s-mp-0", clusterName),
			"lb1":       fmt.Sprintf("%s-mp-1", clusterName),
			"lb2":       fmt.Sprintf("%s-mp-2", clusterName),
		}

		for _, svcIP := range svcIPs {
			lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
			lbName := ptr.Deref(lb.Name, "")
			for _, bp := range lb.Properties.BackendAddressPools {
				for _, a := range bp.Properties.LoadBalancerBackendAddresses {
					nodeName := ptr.Deref(a.Name, "")
					expectedVMSSName := lbNameToExpectedVMSetNameMap[lbName]
					if nodeName == "" || !strings.HasPrefix(nodeName, expectedVMSSName) {
						Fail(fmt.Sprintf("Node %s is not in the expected VMSS %s on LB %s", nodeName, expectedVMSSName, lbName))
					}
				}
			}
		}
	})

	It("should arrange services across load balancers correctly", func() {
		By("Creating a service targeting to lb-2")
		var svcIPs []*string
		l := map[string]string{
			"app": testServiceName,
			"a":   "b",
		}
		svcWithLabel := utils.CreateLoadBalancerServiceManifest("svc-with-label", nil, l, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svcWithLabel, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svsWithLabel, err := utils.WaitServiceExposure(cs, ns.Name, "svc-with-label", []*string{})
		Expect(err).NotTo(HaveOccurred())
		svcIP := svsWithLabel.Status.LoadBalancer.Ingress[0].IP
		svcIPs = append(svcIPs, &svcIP)
		lb := getAzureLoadBalancerFromPIP(tc, &svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		Expect(ptr.Deref(lb.Name, "")).To(Equal("lb-2"))

		svcCount := 2
		var svc *v1.Service
		By(fmt.Sprintf("Creating %d services", svcCount))
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc = utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		By("Checking the load balancer count to equal 3")
		interval := 5 * time.Second
		timeout := 5 * time.Minute
		_, err = waitLBCountEqualTo(tc, interval, timeout, 3, svcIPs)
		Expect(err).NotTo(HaveOccurred())

		clusterName := os.Getenv("CLUSTER_NAME")
		By(fmt.Sprintf("Updating service %s to move to another load balancer %s", svc.Name, clusterName))
		updateServiceAnnotation(svc, map[string]string{"service.beta.kubernetes.io/azure-load-balancer-configurations": clusterName})
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the load balancer count to equal 2")
		lbNames, err := waitLBCountEqualTo(tc, interval, timeout, 2, svcIPs)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbNames.Has("lb-1")).To(BeFalse())
		Expect(lbNames.Has(clusterName)).To(BeTrue())
	})

	It("should arrange local services", func() {
		By("Creating a local service")
		svc := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(ips)).NotTo(BeZero())

		nodeNames, err := getDeploymentPodsNodeNames(cs, ns.Name, testDeploymentName)
		Expect(err).NotTo(HaveOccurred())
		By(fmt.Sprintf("Checking the node count in the local service backend pool to equal %d", len(nodeNames)))
		clusterName := os.Getenv("CLUSTER_NAME")
		Expect(err).NotTo(HaveOccurred())
		expectedBPName := fmt.Sprintf("%s-%s", svc.Namespace, svc.Name)
		err = checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, len(nodeNames))
		Expect(err).NotTo(HaveOccurred())

		By("Scaling the deployment to 3 replicas and then to 1")
		deployment.Spec.Replicas = ptr.To(int32(3))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		deployment.Spec.Replicas = ptr.To(int32(1))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			if err := checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, 1); err != nil {
				if strings.Contains(err.Error(), "expected node count") {
					utils.Logf("Waiting for the node count in the backend pool to equal 1")
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Scaling the deployment to 5")
		deployment.Spec.Replicas = ptr.To(int32(5))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		nodeNames, err = getDeploymentPodsNodeNames(cs, ns.Name, testDeploymentName)
		Expect(err).NotTo(HaveOccurred())
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			if err := checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, len(nodeNames)); err != nil {
				if strings.Contains(err.Error(), "expected node count") {
					utils.Logf("Waiting for the node count in the backend pool to equal %d", len(nodeNames))
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// getDeploymentPodsNodeNames returns the node names of the pods in the deployment created in BeforeEach.
func getDeploymentPodsNodeNames(kubeClient clientset.Interface, namespace, deploymentName string) (map[string]bool, error) {
	var (
		podList *v1.PodList
		res     = make(map[string]bool)
		err     error
	)
	err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
		podList, err = kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, deploymentName) {
				if pod.Spec.NodeName == "" {
					utils.Logf("Waiting for pod %s to be running", pod.Name)
					return false, nil
				}
				utils.Logf("Pod %s is running on node %s", pod.Name, pod.Spec.NodeName)
				res[pod.Spec.NodeName] = true
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func checkNodeCountInBackendPoolByServiceIPs(tc *utils.AzureTestClient, expectedLBName, bpName string, svcIPs []*string, expectedCount int) error {
	for _, svcIP := range svcIPs {
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		lbName := ptr.Deref(lb.Name, "")
		if !strings.EqualFold(lbName, expectedLBName) {
			return fmt.Errorf("expected load balancer name %s, actual %s", expectedLBName, lbName)
		}

		var found bool
		for _, bp := range lb.Properties.BackendAddressPools {
			if strings.HasPrefix(strings.ToLower(ptr.Deref(bp.Name, "")), strings.ToLower(bpName)) {
				found = true
			}
			if len(bp.Properties.LoadBalancerBackendAddresses) != expectedCount {
				return fmt.Errorf("expected node count %d, actual %d", expectedCount, len(bp.Properties.LoadBalancerBackendAddresses))
			}
		}
		if !found {
			return fmt.Errorf("cannot find backend pool %s in load balancer %s", bpName, lbName)
		}
	}
	return nil
}

func getLBsFromPublicIPs(tc *utils.AzureTestClient, pips []*string) sets.Set[string] {
	lbNames := sets.New[string]()
	for _, svcIP := range pips {
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		lbName := ptr.Deref(lb.Name, "")
		lbNames.Insert(lbName)
	}
	return lbNames
}

func waitLBCountEqualTo(tc *utils.AzureTestClient, interval, timeout time.Duration, expectedCount int, svcIPs []*string) (sets.Set[string], error) {
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
