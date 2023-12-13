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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Shared Health Probe", Label(utils.TestSuiteLabelSharedHealthProbe), func() {
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

	It("should use the shared health probe for all cluster services", func() {
		By("Creating and waiting for the exposure of the first cluster service")
		svc1 := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the health probe")
		ctxWithTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err = wait.PollUntilContextCancel(ctxWithTimeout, 5*time.Second, false, func(ctx context.Context) (bool, error) {
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ips[0], "")
			probes := lb.Properties.Probes
			if len(probes) != 1 || !strings.EqualFold(pointer.StringDeref(probes[0].Name, ""), "cluster-service-shared-health-probe") {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Creating and waiting for the exposure of the second cluster service")
		svc2 := utils.CreateLoadBalancerServiceManifest("svc2", serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, "svc2", []*string{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the health probe after creating the second service")
		ctxWithTimeout, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err = wait.PollUntilContextCancel(ctxWithTimeout, 5*time.Second, false, func(ctx context.Context) (bool, error) {
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ips[0], "")
			probes := lb.Properties.Probes
			if len(probes) != 1 || !strings.EqualFold(pointer.StringDeref(probes[0].Name, ""), "cluster-service-shared-health-probe") {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the connectivity of the first service after creating the second service")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())

		By("Creating and waiting for the exposure of a local service")
		svc3 := utils.CreateLoadBalancerServiceManifest("svc3", serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		svc3.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc3, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ips, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, "svc3", []*string{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the health probe after creating the local service")
		ctxWithTimeout, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err = wait.PollUntilContextCancel(ctxWithTimeout, 5*time.Second, false, func(ctx context.Context) (bool, error) {
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ips[0], "")
			probes := lb.Properties.Probes
			if len(probes) != 2 {
				return false, nil
			}
			var foundSharedProbe bool
			for _, probe := range probes {
				if strings.EqualFold(*probe.Name, "cluster-service-shared-health-probe") {
					foundSharedProbe = true
					break
				}
			}
			if !foundSharedProbe {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Removing the first service")
		err = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), testServiceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the health probe after removing the first service")
		ctxWithTimeout, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err = wait.PollUntilContextCancel(ctxWithTimeout, 5*time.Second, false, func(ctx context.Context) (bool, error) {
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ips[0], "")
			probes := lb.Properties.Probes
			if len(probes) != 2 {
				return false, nil
			}
			var foundSharedProbe bool
			for _, probe := range probes {
				if strings.EqualFold(*probe.Name, "cluster-service-shared-health-probe") {
					foundSharedProbe = true
					break
				}
			}
			if !foundSharedProbe {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Removing the second service")
		err = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), "svc2", metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the health probe after removing the second service")
		ctxWithTimeout, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err = wait.PollUntilContextCancel(ctxWithTimeout, 5*time.Second, false, func(ctx context.Context) (bool, error) {
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ips[0], "")
			probes := lb.Properties.Probes
			if len(probes) != 1 || strings.EqualFold(pointer.StringDeref(probes[0].Name, ""), "cluster-service-shared-health-probe") {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
