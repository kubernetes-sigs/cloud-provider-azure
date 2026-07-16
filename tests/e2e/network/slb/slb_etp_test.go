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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// A LoadBalancer service can be created with either ExternalTrafficPolicy: Cluster or Local. The
// difftracker programs a PodIP backend pool either way, but the two policies are distinct service
// shapes that must both provision an Azure LB + public IP and register every backing pod. This
// spec asserts that cloud-provider-owned contract for both policies. It deliberately does NOT
// assert live reachability, which depends on environment dataplane behavior unrelated to the
// cloud provider.
var _ = Describe("SLB - ExternalTrafficPolicy Provisioning", Label(slbTestLabel), func() {
	basename := "slb-etp-test"

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
			Expect(utils.DeleteNamespace(cs, ns.Name)).To(Succeed())

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

	for _, tc := range []struct {
		name   string
		slug   string
		policy v1.ServiceExternalTrafficPolicyType
	}{
		{name: "Cluster", slug: "cluster", policy: v1.ServiceExternalTrafficPolicyTypeCluster},
		{name: "Local", slug: "local", policy: v1.ServiceExternalTrafficPolicyTypeLocal},
	} {
		tc := tc
		It(fmt.Sprintf("should provision a LoadBalancer and register all pods with ExternalTrafficPolicy=%s", tc.name), func() {
			const (
				numPods          = 3
				servicePort      = int32(80)
				targetPort       = 8080
				provisionTimeout = 90 * time.Second
			)

			serviceName := fmt.Sprintf("etp-%s-svc", tc.slug)
			serviceLabels := map[string]string{"app": serviceName}

			By(fmt.Sprintf("Creating %d backend pods", numPods))
			for i := 0; i < numPods; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
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

			By("Waiting for pods to be ready")
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

			By(fmt.Sprintf("Creating a LoadBalancer service with ExternalTrafficPolicy=%s", tc.name))
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: tc.policy,
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
			utils.Logf("Created service %s (UID %s) with ExternalTrafficPolicy=%s", serviceName, serviceUID, tc.name)

			By("Verifying the Azure LB/PIP/Service Gateway entry and all pod registrations")
			// serviceReconciledErr asserts the PIP, LB (SKU=Service + backend pool) and Service
			// Gateway entry all exist and exactly numPods pod IPs are registered.
			eventuallyServiceReconciled(serviceUID, numPods, provisionTimeout)

			By("Verifying the service is assigned an external IP")
			var externalIP string
			Eventually(func() (string, error) {
				svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
				if getErr != nil {
					return "", getErr
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 {
					externalIP = svc.Status.LoadBalancer.Ingress[0].IP
				}
				return externalIP, nil
			}, provisionTimeout, defaultPollInterval).ShouldNot(BeEmpty(),
				"the LoadBalancer service should be assigned an external IP")

			utils.Logf("\n✓ ExternalTrafficPolicy=%s provisioned: external IP %s, %d pod(s) registered",
				tc.name, externalIP, numPods)
		})
	}
})
