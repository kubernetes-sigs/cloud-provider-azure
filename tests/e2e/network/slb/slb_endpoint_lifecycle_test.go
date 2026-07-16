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

// These specs exercise the inbound endpoint registration lifecycle independently of the service
// lifecycle: the Azure LB/PIP/Service Gateway entry stays put while the set of backing pod IPs
// (the registered address locations) churns underneath it. They assert only the cloud-provider
// contract (which addresses are registered), not live traffic.
var _ = Describe("SLB - Endpoint Lifecycle", Label(slbTestLabel), func() {
	basename := "slb-endpoint-lifecycle-test"

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

	// makeNetexecPod returns a plain agnhost netexec pod (ready as soon as it is running).
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

	newLBService := func(name string, labels map[string]string, port int32, targetPort int) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{{
					Port:       port,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
	}

	It("should empty the backend when all pods are deleted and repopulate it when they return", func() {
		const (
			numPods     = 4
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "churn-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating the service and %d pods", numPods))
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), newLBService(serviceName, labels, servicePort, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for all pods to register")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By("Deleting all backend pods")
		Expect(cs.CoreV1().Pods(ns.Name).DeleteCollection(context.TODO(), metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: "app=" + serviceName})).To(Succeed())

		By("Verifying the backend empties while the LB/Service Gateway entry remains")
		Eventually(func() (int, error) {
			return countRegisteredEndpoints(serviceUID)
		}, waitTime, defaultPollInterval).Should(Equal(0), "all pod registrations should drain")
		Expect(verifyAzureResources(serviceUID)).To(Succeed(),
			"the LB/PIP/Service Gateway entry must survive an empty backend")

		By(fmt.Sprintf("Recreating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-new-%d", serviceName, i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Verifying the backend repopulates")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ Endpoint churn to zero and back verified")
	})

	It("should provision an empty-backed LB before any pods exist, then register pods when they appear", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "preprovision-service"
		labels := map[string]string{"app": serviceName}

		By("Creating the service with no matching pods")
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), newLBService(serviceName, labels, servicePort, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)

		By("Verifying the LB/PIP/Service Gateway entry provisions with an empty backend")
		Eventually(func() error {
			if err := verifyAzureResources(serviceUID); err != nil {
				return err
			}
			n, err := countRegisteredEndpoints(serviceUID)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("expected 0 registered endpoints before pods exist, got %d", n)
			}
			return nil
		}, waitTime, defaultPollInterval).Should(Succeed(),
			"the LB should provision with an empty backend before pods exist")

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Verifying the pods register once they appear")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ Pre-provisioned LB registered pods on appearance")
	})

	It("should deregister a pod when it becomes NotReady and re-register it when Ready again", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 120 * time.Second
		)
		serviceName := "readiness-service"
		labels := map[string]string{"app": serviceName}

		// A file-gated readiness probe lets the test flip a single pod Ready<->NotReady without
		// deleting it: the container is Ready while /tmp/ready exists. netexec stays PID 1.
		readinessPod := func(name string) *v1.Pod {
			p := makeNetexecPod(name, labels, targetPort)
			p.Spec.Containers[0].Command = []string{"/bin/sh", "-c",
				fmt.Sprintf("touch /tmp/ready && exec /agnhost netexec --http-port=%d", targetPort)}
			p.Spec.Containers[0].Args = nil
			p.Spec.Containers[0].ReadinessProbe = &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					Exec: &v1.ExecAction{Command: []string{"/bin/sh", "-c", "test -f /tmp/ready"}},
				},
				InitialDelaySeconds: 2,
				PeriodSeconds:       3,
				FailureThreshold:    1,
				SuccessThreshold:    1,
			}
			return p
		}

		By(fmt.Sprintf("Creating the service and %d file-gated pods", numPods))
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), newLBService(serviceName, labels, servicePort, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		flipPod := fmt.Sprintf("%s-pod-0", serviceName)
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), readinessPod(fmt.Sprintf("%s-pod-%d", serviceName, i)), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for all pods to register")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By(fmt.Sprintf("Making pod %s NotReady (removing its readiness file)", flipPod))
		_, err = utils.RunKubectl(ns.Name, "exec", flipPod, "--", "/bin/sh", "-c", "rm -f /tmp/ready")
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the NotReady pod is deregistered")
		Eventually(func() (int, error) {
			return countRegisteredEndpoints(serviceUID)
		}, waitTime, defaultPollInterval).Should(Equal(numPods-1),
			"a NotReady pod should be deregistered from the backend")

		By(fmt.Sprintf("Making pod %s Ready again (restoring its readiness file)", flipPod))
		_, err = utils.RunKubectl(ns.Name, "exec", flipPod, "--", "/bin/sh", "-c", "touch /tmp/ready")
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the pod is re-registered")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ Readiness-driven deregister/re-register verified")
	})
})
