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

// Edge cases around the inbound service shape: a service with many distinct ports, and several
// services that select the same pods. Both assert the cloud-provider contract (LB rules and pod
// registrations) and are independent of the environment dataplane.
var _ = Describe("SLB - Service Config Edge Cases", Label(slbTestLabel), func() {
	basename := "slb-service-config-test"

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

	makeNetexecPod := func(name string, labels map[string]string, targetPort int) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}},
			},
		}
	}

	It("should create one LB rule per port for a service with many distinct ports", func() {
		const (
			numPods    = 2
			numPorts   = 6
			basePort   = int32(8000)
			baseTarget = 9000
			waitTime   = 90 * time.Second
		)
		serviceName := "many-ports-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			// Pods only need to be Ready to register as endpoints; LB rules come from the
			// Service spec, so the pods do not have to listen on every target port.
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, baseTarget), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating a service with %d distinct ports", numPorts))
		ports := make([]v1.ServicePort, 0, numPorts)
		wantPorts := make([]int32, 0, numPorts)
		for i := 0; i < numPorts; i++ {
			fePort := basePort + int32(i)
			ports = append(ports, v1.ServicePort{
				Name:       fmt.Sprintf("p%d", i),
				Port:       fePort,
				TargetPort: intstr.FromInt(baseTarget + i),
				Protocol:   v1.ProtocolTCP,
			})
			wantPorts = append(wantPorts, fePort)
		}
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    ports,
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)

		By("Waiting for the service to provision and register its pods")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By(fmt.Sprintf("Verifying the LB has exactly %d rules, one per service port", numPorts))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 60*time.Second, 5*time.Second).Should(Equal(wantPorts),
			"the LB must have one rule per service port")

		utils.Logf("\n✓ Many-port service produced %d LB rules", numPorts)
	})

	It("should let two services that select the same pods each register those pods", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		labels := map[string]string{"app": "shared-backend"}

		By(fmt.Sprintf("Creating %d shared pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("shared-pod-%d", i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		newSharedService := func(name string) string {
			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
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
			created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			return string(created.UID)
		}

		By("Creating two LoadBalancer services that select the same pods")
		uidA := newSharedService("shared-svc-a")
		uidB := newSharedService("shared-svc-b")

		By("Verifying each service independently provisions and registers all shared pods")
		eventuallyServiceReconciled(uidA, numPods, waitTime)
		eventuallyServiceReconciled(uidB, numPods, waitTime)

		utils.Logf("\n✓ Shared-pod services each registered %d pods (UIDs %s, %s)", numPods, uidA, uidB)
	})
})
