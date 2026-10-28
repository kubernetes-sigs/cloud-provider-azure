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
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// CCM Crash Recovery test label
const slbCrashTestLabel = "SLB-Crash"

var _ = Describe("Container Load Balancer Crash Recovery", Label(slbTestLabel, slbCrashTestLabel), func() {
	basename := "slb-crash-test"
	serviceName := "crash-service"

	var (
		cs        clientset.Interface
		ccmClient *CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error

		// First check if CCM cluster is configured
		if !IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM crash tests: %s environment variable not set", CCMKubeconfigEnvVar))
		}

		// Create workload cluster client
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		// Create CCM cluster client
		ccmClient, err = NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		// Create test namespace in workload cluster
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		// Verify CCM is running before starting test
		ctx := context.Background()
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Azure cleanup to complete")
			eventuallyAzureCleanup(2 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		// Ensure CCM is recovered before next test
		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		cs = nil
		ccmClient = nil
		ns = nil
	})

	It("should recover service after CCM crash during stable state", func() {
		const (
			numPods     = 5
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for pods to be ready")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		utils.Logf("Service UID: %s", serviceUID)
		Eventually(func() error {
			return verifyAzureResources(serviceUID)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"Azure resources should exist before CCM crash")

		By("Crashing CCM and waiting for recovery")
		err = ccmClient.CrashCCMAndWaitForRecovery(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile service")
		Eventually(func() error {
			if err := verifyAzureResources(serviceUID); err != nil {
				return fmt.Errorf("verify Azure resources after CCM recovery: %w", err)
			}

			endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get endpoints %s: %w", serviceName, err)
			}

			totalAddresses := 0
			for _, subset := range endpoints.Subsets {
				totalAddresses += len(subset.Addresses)
			}
			utils.Logf("Endpoint count after CCM recovery: %d (expected: %d)", totalAddresses, numPods)
			if totalAddresses != numPods {
				return fmt.Errorf("got %d endpoints after CCM recovery, want %d", totalAddresses, numPods)
			}
			return nil
		}, 30*time.Second, 10*time.Second).Should(Succeed(),
			"Azure resources and endpoints should persist after CCM recovery")
	})

	It("should handle pod creation during CCM downtime", func() {
		const (
			initialPods    = 3
			additionalPods = 2
			servicePort    = int32(8080)
			targetPort     = 8080
			waitTime       = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating initial pods")
		for i := 0; i < initialPods; i++ {
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for initial pods to be ready")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		Eventually(func() error {
			return verifyAzureResources(serviceUID)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"Azure resources should exist before CCM crash")

		By("Crashing CCM and waiting until it is genuinely down")
		// DeleteAllCCMPods only issues the deletes, so the pods created below could otherwise be
		// observed by a still-running controller - the spec would then prove ordinary reconciliation
		// rather than recovery of work missed during downtime.
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Creating additional pods while CCM is down")
		for i := initialPods; i < initialPods+additionalPods; i++ {
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d additional pods while CCM was down", additionalPods)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile endpoints")
		expectedPods := initialPods + additionalPods
		Eventually(func() error {
			if err := verifyAzureResources(serviceUID); err != nil {
				return fmt.Errorf("verify Azure resources after CCM recovery: %w", err)
			}

			endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get endpoints %s: %w", serviceName, err)
			}

			totalAddresses := 0
			for _, subset := range endpoints.Subsets {
				totalAddresses += len(subset.Addresses)
			}
			utils.Logf("Expected %d endpoints, found %d", expectedPods, totalAddresses)
			if totalAddresses != expectedPods {
				return fmt.Errorf("got %d endpoints after CCM recovery, want %d", totalAddresses, expectedPods)
			}
			return nil
		}, 60*time.Second, 10*time.Second).Should(Succeed(),
			"Azure resources and all endpoints should be reconciled after CCM recovery")
	})

	It("should maintain consistency after multiple CCM crashes", func() {
		const (
			numPods     = 5
			servicePort = int32(8080)
			targetPort  = 8080
			numCrashes  = 3
			waitTime    = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for pods to be ready")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		Eventually(func() error {
			return verifyAzureResources(serviceUID)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"Azure resources should exist before CCM crashes")

		By(fmt.Sprintf("Performing %d CCM crashes and recoveries", numCrashes))
		for i := 1; i <= numCrashes; i++ {
			utils.Logf("=== CCM Crash iteration %d/%d ===", i, numCrashes)

			err = ccmClient.CrashCCMAndWaitForRecovery(ctx, CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Waiting for CCM to reconcile after crash %d", i))
			Eventually(func() error {
				return verifyAzureResources(serviceUID)
			}, 30*time.Second, 10*time.Second).Should(Succeed(),
				"Azure resources should persist after crash %d", i)
		}

		By("Verifying Azure resources after all crashes")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should still exist after multiple CCM crashes")

		By("Verifying endpoints are correct")
		endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		totalAddresses := 0
		for _, subset := range endpoints.Subsets {
			totalAddresses += len(subset.Addresses)
		}
		utils.Logf("Final endpoint count: %d (expected: %d)", totalAddresses, numPods)
		Expect(totalAddresses).To(Equal(numPods), "All pods should be in endpoints after multiple crashes")
	})

	It("should remove endpoints that were scaled away while the CCM was starting up", func() {
		const (
			initialReplicas  = 6
			scaledReplicas   = 2
			extraServices    = 20
			servicePort      = int32(8080)
			targetPort       = 8080
			provisionTimeout = 3 * time.Minute
			listTimeout      = 3 * time.Minute
			initTimeout      = 10 * time.Minute
			reconcileTimeout = 5 * time.Minute
			// Deleting the additional services takes longer than the shared AfterEach allows.
			extraCleanupTimeout = 8 * time.Minute
			// Logged by the restarted CCM after its startup EndpointSlice LIST, and when its blocking
			// initial sync returns; its shared informers start only after the latter.
			endpointSlicesListedLog = `"Processed Kubernetes EndpointSlices"`
			initCompletedLog        = `"Completed initialization"`
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By(fmt.Sprintf("Creating a deployment with %d replicas", initialReplicas))
		replicas := int32(initialReplicas)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: serviceLabels},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: serviceLabels},
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
				},
			},
		}
		_, err := cs.AppsV1().Deployments(ns.Name).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for pods to be ready")
		Eventually(func() (int32, error) {
			d, err := cs.AppsV1().Deployments(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				return 0, err
			}
			return d.Status.ReadyReplicas, nil
		}, provisionTimeout, 5*time.Second).Should(Equal(replicas),
			"deployment %s should have %d ready replicas", serviceName, initialReplicas)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		createdService, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)
		utils.Logf("Service UID: %s", serviceUID)

		By(fmt.Sprintf("Waiting for the Service Gateway to register exactly the %d ready endpoints", initialReplicas))
		Eventually(func() error {
			want, err := readyEndpointIPs(cs, ns.Name, serviceName)
			if err != nil {
				return err
			}
			if len(want) != initialReplicas {
				return fmt.Errorf("service %s has %d ready endpoint addresses, want %d", serviceName, len(want), initialReplicas)
			}
			return serviceReconciledMatchErr(serviceUID, want)
		}, provisionTimeout, defaultPollInterval).Should(Succeed(),
			"Service Gateway should register every ready endpoint before the CCM crash")

		By("Crashing CCM and waiting until it is genuinely down")
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d additional LoadBalancer services while CCM is down", extraServices))
		// The restarted CCM must provision these inside its blocking initial sync, which widens the
		// window between its startup EndpointSlice LIST and the start of its informers.
		var extraServiceNames, extraServiceUIDs []string
		// A Go defer, unlike DeferCleanup, runs before the shared AfterEach and on every exit path,
		// including Skip.
		defer func() {
			By(fmt.Sprintf("Deleting the %d additional services and waiting for the Service Gateway to drop them", len(extraServiceNames)))
			for _, name := range extraServiceNames {
				Expect(cs.CoreV1().Services(ns.Name).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
			}
			Eventually(func() error {
				return inboundServicesCleanupErr(extraServiceUIDs)
			}, extraCleanupTimeout, defaultPollInterval).Should(Succeed(),
				"the additional services should be removed from the Service Gateway")
		}()
		for i := 0; i < extraServices; i++ {
			extraService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-extra-%d", serviceName, i),
					Namespace: ns.Name,
					Annotations: map[string]string{
						"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
					},
				},
				Spec: v1.ServiceSpec{
					Type:     v1.ServiceTypeLoadBalancer,
					Selector: serviceLabels,
					Ports: []v1.ServicePort{
						{
							Name:       "http",
							Protocol:   v1.ProtocolTCP,
							Port:       servicePort + int32(i) + 1,
							TargetPort: intstr.FromInt(targetPort),
						},
					},
				},
			}
			createdExtra, err := cs.CoreV1().Services(ns.Name).Create(ctx, extraService, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			extraServiceNames = append(extraServiceNames, createdExtra.Name)
			extraServiceUIDs = append(extraServiceUIDs, string(createdExtra.UID))
		}

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the restarted CCM to list EndpointSlices")
		// Scaling as soon as the LIST is logged, rather than after a fixed delay, keeps the scale-down
		// inside the initial sync even when that sync is short.
		var ccmPodName string
		err = wait.PollUntilContextTimeout(ctx, time.Second, listTimeout, true, func(ctx context.Context) (bool, error) {
			pods, err := ccmClient.GetCCMPods(ctx)
			if err != nil {
				return false, nil
			}
			for _, pod := range pods {
				if slices.Contains(crashedUIDs, pod.UID) {
					continue
				}
				logs, err := ccmClient.GetCCMLogs(ctx, pod.Name, ccmContainerName)
				if err != nil {
					continue
				}
				if _, found := ccmLogLineTime(logs, endpointSlicesListedLog); found {
					ccmPodName = pod.Name
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			Skip(fmt.Sprintf("scale-down could not be placed inside the CCM startup window; no restarted CCM pod logged %s within %v",
				endpointSlicesListedLog, listTimeout))
		}

		By(fmt.Sprintf("Scaling the deployment to %d replicas while CCM pod %s is in its initial sync", scaledReplicas, ccmPodName))
		scaledAt := time.Now()
		_, err = cs.AppsV1().Deployments(ns.Name).UpdateScale(ctx, serviceName, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: scaledReplicas,
			},
		}, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the scaled-away endpoints to stop being ready")
		Eventually(func() (int, error) {
			addrs, err := readyEndpointIPs(cs, ns.Name, serviceName)
			return len(addrs), err
		}, time.Minute, time.Second).Should(Equal(scaledReplicas),
			"service %s should have %d ready endpoint addresses after the scale-down", serviceName, scaledReplicas)
		landedAt := time.Now()
		utils.Logf("Scale-down issued at %s; EndpointSlices reflected it by %s",
			scaledAt.Format(time.RFC3339Nano), landedAt.Format(time.RFC3339Nano))

		By("Waiting for the restarted CCM to complete its initial sync")
		var listedAt, initCompletedAt time.Time
		err = wait.PollUntilContextTimeout(ctx, CCMRecoveryPollInterval, initTimeout, true, func(ctx context.Context) (bool, error) {
			logs, err := ccmClient.GetCCMLogs(ctx, ccmPodName, ccmContainerName)
			if err != nil {
				return false, nil
			}
			var listed, completed bool
			listedAt, listed = ccmLogLineTime(logs, endpointSlicesListedLog)
			initCompletedAt, completed = ccmLogLineTime(logs, initCompletedLog)
			return listed && completed, nil
		})
		if err != nil {
			Skip(fmt.Sprintf("scale-down did not land inside the CCM startup window; CCM pod %s did not log %s within %v",
				ccmPodName, initCompletedLog, initTimeout))
		}
		utils.Logf("CCM pod %s listed EndpointSlices at %s and completed initialization at %s",
			ccmPodName, listedAt.Format(time.RFC3339Nano), initCompletedAt.Format(time.RFC3339Nano))

		// Checked before the assertion, so a stale registration fails the spec whenever the window was hit.
		// The bound is landedAt: a change the informers' initial list misses arrives as an ordinary
		// Update, which removes the endpoints even without the fix.
		if !listedAt.Before(scaledAt) || !landedAt.Before(initCompletedAt) {
			Skip(fmt.Sprintf("scale-down did not land inside the CCM startup window; it ran from %s to %s, but CCM pod %s "+
				"listed EndpointSlices at %s and completed initialization at %s; raise extraServices to lengthen the initial sync",
				scaledAt.Format(time.RFC3339Nano), landedAt.Format(time.RFC3339Nano), ccmPodName,
				listedAt.Format(time.RFC3339Nano), initCompletedAt.Format(time.RFC3339Nano)))
		}

		By(fmt.Sprintf("Verifying the Service Gateway holds exactly the %d remaining ready endpoints", scaledReplicas))
		Eventually(func() error {
			want, err := readyEndpointIPs(cs, ns.Name, serviceName)
			if err != nil {
				return err
			}
			if len(want) != scaledReplicas {
				return fmt.Errorf("service %s has %d ready endpoint addresses, want %d", serviceName, len(want), scaledReplicas)
			}
			return serviceReconciledMatchErr(serviceUID, want)
		}, reconcileTimeout, defaultPollInterval).Should(Succeed(),
			"endpoints scaled away while the CCM was starting up must not stay registered in the Service Gateway")

		utils.Logf("✓ Endpoints scaled away during CCM startup were removed from the Service Gateway")
	})
})

var _ = Describe("Container Load Balancer Outbound Crash Recovery", Label(slbTestLabel, slbCrashTestLabel), func() {
	basename := "slb-outbound-crash"

	var (
		cs        clientset.Interface
		ccmClient *CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error

		if !IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM crash tests: %s environment variable not set", CCMKubeconfigEnvVar))
		}

		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ccmClient, err = NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		ctx := context.Background()
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Azure cleanup to complete")
			eventuallyAzureCleanup(6 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		cs = nil
		ccmClient = nil
		ns = nil
	})

	It("should recover NAT gateway after CCM crash", func() {
		const (
			numPods    = 5
			egressName = "test-egress-crash"
			targetPort = 8080
			waitTime   = 90 * time.Second
		)

		ctx := context.Background()

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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for NAT Gateway provisioning")
		var natGatewayID string
		Eventually(func() error {
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				return fmt.Errorf("query Service Gateway services: %w", err)
			}

			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
					natGatewayID = svc.Properties.PublicNatGatewayID
					utils.Logf("Found outbound service '%s' with NAT Gateway: %s", egressName, natGatewayID)
					if natGatewayID == "" {
						return fmt.Errorf("NAT Gateway ID should not be empty")
					}
					return nil
				}
			}
			return fmt.Errorf("outbound service %s should exist before crash", egressName)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"outbound service should exist with a NAT Gateway before crash")

		By("Crashing CCM and waiting for recovery")
		err := ccmClient.CrashCCMAndWaitForRecovery(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile NAT Gateway")
		Eventually(func() error {
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				return fmt.Errorf("query Service Gateway services: %w", err)
			}

			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
					recoveredNatGatewayID := svc.Properties.PublicNatGatewayID
					utils.Logf("Outbound service still exists after recovery with NAT Gateway: %s", recoveredNatGatewayID)
					if recoveredNatGatewayID != natGatewayID {
						return fmt.Errorf("NAT Gateway ID after recovery = %q, want %q", recoveredNatGatewayID, natGatewayID)
					}

					registeredPods, err := countRegisteredEndpoints(egressName)
					if err != nil {
						return err
					}
					utils.Logf("Registered %d pod IPs for egress gateway after recovery", registeredPods)
					if registeredPods != numPods {
						return fmt.Errorf("registered pod count after recovery = %d, want %d", registeredPods, numPods)
					}
					return nil
				}
			}
			return fmt.Errorf("outbound service %s should still exist after CCM recovery", egressName)
		}, 30*time.Second, 10*time.Second).Should(Succeed(),
			"NAT Gateway ID and pod registrations should persist after CCM recovery")
	})

	It("should not strand egress pod finalizers when pods are deleted during CCM downtime", func() {
		const (
			numPods    = 3
			egressName = "egress-strand-crash"
			targetPort = 8080
			waitTime   = 90 * time.Second
		)
		ctx := context.Background()
		egressSelector := egressLabel + "=" + egressName

		By(fmt.Sprintf("Creating %d egress pods", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-strand-pod-%d", i),
					Namespace: ns.Name,
					Labels:    map[string]string{egressLabel: egressName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "test-app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for the NAT gateway to provision and pods to register")
		eventuallyEgressRegistered(egressName, numPods, waitTime)

		// Delete the egress pods, then immediately crash CCM so the in-flight pod-deletion tracking
		// (the in-memory pendingPodDeletions) is lost. On recovery those pods are mid-deletion but no
		// longer in live engine state, so DeletePod takes its stale-pod early return and never
		// enqueues them. Their servicegateway.azure.com/pod-cleanup finalizer must still be removed
		// (directly, since there is nothing left to drain from NRP) rather than stranding the pod
		// Terminating forever. This guards the finalizer drain-gating regression.
		By("Deleting the egress pods and immediately crashing CCM to lose the in-flight delete tracking")
		Expect(cs.CoreV1().Pods(ns.Name).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: egressSelector})).To(Succeed())
		// The premise is that the CCM dies while the deletes are in flight, losing its in-memory
		// pendingPodDeletions. Waiting for it to be fully down establishes that; issuing the pod
		// deletes and returning would leave the old controller running long enough to drain them
		// normally, so the recovery path under test would never be exercised.
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the pods are still Terminating with their cleanup finalizer while the CCM is down")
		// Without this the spec cannot distinguish "recovery removed a stranded finalizer" from
		// "the finalizer was already gone before the crash", which is the whole point of the guard.
		stranded, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: egressSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(stranded.Items).NotTo(BeEmpty(),
			"the egress pods must still exist while the CCM is down; their cleanup finalizer should be blocking deletion")
		for i := range stranded.Items {
			Expect(stranded.Items[i].DeletionTimestamp).NotTo(BeNil(),
				"pod %s should be Terminating", stranded.Items[i].Name)
			Expect(hasFinalizer(stranded.Items[i].Finalizers, serviceGatewayPodFinalizer)).To(BeTrue(),
				"pod %s should still carry the cleanup finalizer while the CCM is down", stranded.Items[i].Name)
		}

		By("Waiting for CCM to recover")
		Expect(ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)).To(Succeed())

		By("Verifying the egress pods fully delete (no stranded pod-cleanup finalizer)")
		Eventually(func() (int, error) {
			pods, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: egressSelector})
			if err != nil {
				return -1, err
			}
			for i := range pods.Items {
				p := &pods.Items[i]
				for _, f := range p.Finalizers {
					if f == serviceGatewayPodFinalizer {
						utils.Logf("pod %s still holds %s (deletionTS=%v, phase=%s)", p.Name, serviceGatewayPodFinalizer, p.DeletionTimestamp, p.Status.Phase)
					}
				}
			}
			return len(pods.Items), nil
		}, 4*time.Minute, defaultPollInterval).Should(Equal(0),
			"egress pods deleted during CCM downtime must not be stranded Terminating by a stuck pod-cleanup finalizer")

		utils.Logf("✓ Egress pod finalizers cleared after delete-during-CCM-downtime (no strand)")
	})

	It("should handle pod creation during CCM downtime for NAT gateway", func() {
		const (
			initialPods    = 3
			additionalPods = 2
			egressName     = "test-egress-downtime"
			targetPort     = 8080
			waitTime       = 90 * time.Second
		)

		ctx := context.Background()

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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for NAT Gateway provisioning")
		Eventually(func() error {
			return egressRegisteredErr(egressName, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"outbound service should exist")

		By("Crashing CCM and waiting until it is genuinely down")
		// See above: the "during downtime" premise must be established, not assumed.
		crashedUIDs, err := ccmClient.CrashCCMAndWaitForDown(ctx, CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d additional pods while CCM is down", additionalPods))
		for i := initialPods; i < initialPods+additionalPods; i++ {
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d additional pods while CCM was down", additionalPods)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, CCMRecoveryTimeout, crashedUIDs...)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile")
		expectedPods := initialPods + additionalPods
		Eventually(func() error {
			registeredPods, err := countRegisteredEndpoints(egressName)
			if err != nil {
				return err
			}
			utils.Logf("Expected %d pods, found %d registered", expectedPods, registeredPods)
			if registeredPods != expectedPods {
				return fmt.Errorf("registered pod count after recovery = %d, want %d", registeredPods, expectedPods)
			}
			return nil
		}, 60*time.Second, 10*time.Second).Should(Succeed(),
			"all pods should be registered after CCM recovery")
	})

	It("should maintain multiple NAT gateways across CCM crashes", func() {
		const (
			podsPerGateway = 3
			numCrashes     = 2
			targetPort     = 8080
			waitTime       = 90 * time.Second
		)

		egressGateways := []string{"egress-alpha-crash", "egress-beta-crash"}
		ctx := context.Background()

		By(fmt.Sprintf("Creating %d egress gateways with %d pods each", len(egressGateways), podsPerGateway))
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Waiting for NAT Gateways provisioning")
		natGatewayIDs := make(map[string]string)
		Eventually(func() error {
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				return fmt.Errorf("query Service Gateway services: %w", err)
			}

			natGatewayIDs = make(map[string]string)
			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" {
					for _, egressName := range egressGateways {
						if svc.Name == egressName {
							natGatewayIDs[egressName] = svc.Properties.PublicNatGatewayID
							utils.Logf("Found egress gateway '%s' with NAT Gateway: %s", egressName, svc.Properties.PublicNatGatewayID)
						}
					}
				}
			}
			if len(natGatewayIDs) != len(egressGateways) {
				return fmt.Errorf("found %d egress gateways, want %d", len(natGatewayIDs), len(egressGateways))
			}
			for _, egressName := range egressGateways {
				if natGatewayIDs[egressName] == "" {
					return fmt.Errorf("egress gateway %s has empty NAT Gateway ID", egressName)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all egress gateways should exist before crashes")

		By(fmt.Sprintf("Performing %d CCM crashes", numCrashes))
		for i := 1; i <= numCrashes; i++ {
			utils.Logf("=== CCM Crash iteration %d/%d ===", i, numCrashes)

			err := ccmClient.CrashCCMAndWaitForRecovery(ctx, CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Waiting for reconciliation after crash %d", i))
			Eventually(func() error {
				sgResponse, err := queryServiceGatewayServices()
				if err != nil {
					return fmt.Errorf("query Service Gateway services: %w", err)
				}

				foundGateways := 0
				for _, svc := range sgResponse.Value {
					if svc.Properties.ServiceType == "Outbound" {
						for egressName, expectedNatID := range natGatewayIDs {
							if svc.Name == egressName {
								foundGateways++
								if svc.Properties.PublicNatGatewayID != expectedNatID {
									return fmt.Errorf("NAT Gateway ID for %s after crash %d = %q, want %q", egressName, i, svc.Properties.PublicNatGatewayID, expectedNatID)
								}
							}
						}
					}
				}
				if foundGateways != len(egressGateways) {
					return fmt.Errorf("found %d egress gateways after crash %d, want %d", foundGateways, i, len(egressGateways))
				}
				return nil
			}, 30*time.Second, 10*time.Second).Should(Succeed(),
				"all egress gateways should persist after crash %d", i)
		}

		By("Verifying final pod registrations")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		for _, egressName := range egressGateways {
			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == egressName {
							registeredPods++
						}
					}
				}
			}
			utils.Logf("Egress gateway '%s' has %d registered pods", egressName, registeredPods)
			Expect(registeredPods).To(Equal(podsPerGateway), "All pods should be registered for %s", egressName)
		}
	})
})

// readyEndpointIPs returns the addresses of the ready endpoints in the Service's EndpointSlices,
// which is the address set the Service Gateway should hold for it.
func readyEndpointIPs(cs clientset.Interface, namespace, serviceName string) (map[string]struct{}, error) {
	endpointSlices, err := cs.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, serviceName),
	})
	if err != nil {
		return nil, fmt.Errorf("list EndpointSlices of service %s: %w", serviceName, err)
	}
	addrs := make(map[string]struct{})
	for _, endpointSlice := range endpointSlices.Items {
		for _, endpoint := range endpointSlice.Endpoints {
			// A nil Ready condition means ready, as in the CCM.
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			for _, addr := range endpoint.Addresses {
				addrs[addr] = struct{}{}
			}
		}
	}
	return addrs, nil
}

// inboundServicesCleanupErr returns nil once none of the given inbound services is registered in the
// Service Gateway or referenced by an address location, while other services may remain.
func inboundServicesCleanupErr(serviceUIDs []string) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	for _, svc := range sgResponse.Value {
		if slices.Contains(serviceUIDs, svc.Name) {
			return fmt.Errorf("service %s still registered in Service Gateway", svc.Name)
		}
	}
	alResponse, err := queryServiceGatewayAddressLocations()
	if err != nil {
		return fmt.Errorf("query Service Gateway address locations: %w", err)
	}
	for _, location := range alResponse.Value {
		for _, addr := range location.Addresses {
			for _, svc := range addr.Services {
				if slices.Contains(serviceUIDs, svc) {
					return fmt.Errorf("address %s in location %s still references service %s",
						addr.Address, location.AddressLocation, svc)
				}
			}
		}
	}
	return nil
}
