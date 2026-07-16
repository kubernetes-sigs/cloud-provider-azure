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

// Mutation edge cases that drive UpdateService through changes the port-number update specs do
// not cover: switching a port's protocol, and switching ExternalTrafficPolicy on a live service.
var _ = Describe("SLB - Mutation Edge Cases", Label(slbTestLabel), func() {
	basename := "slb-mutation-edge-test"

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

	makePods := func(serviceName string, labels map[string]string, numPods, targetPort int) {
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", serviceName, i), Namespace: ns.Name, Labels: labels},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "test-app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
	}

	// updateService applies mutate to the live service, retrying on conflict.
	updateService := func(name string, mutate func(*v1.Service)) {
		Eventually(func() error {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			mutate(svc)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	}

	It("should reconcile when a port's protocol changes from TCP to UDP", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "protocol-change-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods and a TCP LoadBalancer service", numPods))
		makePods(serviceName, labels, numPods, targetPort)
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)

		By("Waiting for the TCP rule to provision")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)
		Eventually(func() (string, error) {
			return frontendPortProtocol(serviceUID, servicePort)
		}, 60*time.Second, 5*time.Second).Should(Equal("Tcp"),
			"the LB should start with a TCP rule")

		By("Changing the port protocol to UDP")
		updateService(serviceName, func(svc *v1.Service) {
			svc.Spec.Ports[0].Protocol = v1.ProtocolUDP
		})

		By("Verifying the LB rule reconciles to UDP")
		Eventually(func() (string, error) {
			return frontendPortProtocol(serviceUID, servicePort)
		}, waitTime, 5*time.Second).Should(Equal("Udp"),
			"the LB rule should become UDP after the protocol change")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ Port protocol change TCP->UDP reconciled")
	})

	It("should stay reconciled when ExternalTrafficPolicy is switched on a live service", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "etp-change-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods and a Cluster-policy LoadBalancer service", numPods))
		makePods(serviceName, labels, numPods, targetPort)
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
				Selector:              labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)

		By("Waiting for initial provisioning")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By("Switching ExternalTrafficPolicy Cluster -> Local")
		updateService(serviceName, func(svc *v1.Service) {
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
		})
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By("Switching ExternalTrafficPolicy Local -> Cluster")
		updateService(serviceName, func(svc *v1.Service) {
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
		})
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ ExternalTrafficPolicy switches stayed reconciled")
	})
})

// frontendPortProtocol returns the Azure protocol ("Tcp"/"Udp") of the LB rule whose frontend
// port matches the given port, for the LB named serviceUID.
func frontendPortProtocol(serviceUID string, frontendPort int32) (string, error) {
	rules, err := getLoadBalancerRules(serviceUID)
	if err != nil {
		return "", err
	}
	for _, r := range rules {
		if r.FrontendPort == frontendPort {
			return r.Protocol, nil
		}
	}
	return "", fmt.Errorf("no LB rule with frontend port %d on %s", frontendPort, serviceUID)
}
