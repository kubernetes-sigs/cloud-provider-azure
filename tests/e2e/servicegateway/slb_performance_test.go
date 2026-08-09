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
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Performance Test", Label(slbTestLabel, "performance"), func() {
	basename := "slb-perf-test"

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

	// Cleanup must run here rather than at the end of the It body: this spec creates 200 services
	// in the one ServiceGateway resource that every spec in the suite shares, so a failure part
	// way through would otherwise leave them registered and turn the next spec's cleanup
	// assertion (which requires only the default outbound service to remain) red as well.
	AfterEach(func() {
		if cs != nil && ns != nil {
			By("Deleting the test namespace")
			if err := utils.DeleteNamespace(cs, ns.Name); err != nil {
				// At this scale namespace deletion regularly outlives its client-side timeout
				// while Azure finalizers drain; the Azure-side wait below is the real assertion.
				utils.Logf("WARNING: namespace deletion did not complete in time: %v", err)
			}

			By("Waiting for Azure cleanup")
			eventuallyAzureCleanup(15 * time.Minute)
		}
		cs = nil
		ns = nil
	})

	It("should create and delete 200 LoadBalancer services in parallel", NodeTimeout(4*time.Hour), func(ctx SpecContext) {
		const (
			numServices = 200
			servicePort = int32(8080)
			targetPort  = 8080
		)

		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("SLB PERFORMANCE TEST: %d SERVICES - PARALLEL CREATE/DELETE", numServices)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("Test started at: %s", time.Now().Format(time.RFC3339))

		// Track service UIDs
		serviceUIDs := make(map[string]bool)
		var uidMutex sync.Mutex

		// ============================================
		// PHASE 1: CREATE ALL SERVICES + PODS IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 1: Creating %d services + pods in parallel ---", numServices)
		createStart := time.Now()

		var wg sync.WaitGroup
		// Assertions cannot run inside these goroutines (there is no GinkgoRecover, so a failure
		// would panic the worker instead of failing the spec), and simply logging an error lets the
		// run continue with fewer services than it claims to be measuring. Collect failures and
		// assert once the workers have joined.
		var createErrs []string
		for i := 0; i < numServices; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				serviceName := fmt.Sprintf("perf-svc-%d", index)
				podName := fmt.Sprintf("perf-pod-%d", index)
				labels := map[string]string{"app": serviceName}

				// Create Service
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Selector:              labels,
						Ports:                 []v1.ServicePort{{Port: servicePort, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP}},
					},
				}

				createdSvc, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
				if err != nil {
					uidMutex.Lock()
					createErrs = append(createErrs, fmt.Sprintf("service perf-svc-%d: %v", index, err))
					uidMutex.Unlock()
					return
				}

				uidMutex.Lock()
				serviceUIDs[string(createdSvc.UID)] = false // false = not yet provisioned
				uidMutex.Unlock()

				// Create Pod
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns.Name, Labels: labels},
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}}},
					},
				}
				if _, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{}); err != nil {
					uidMutex.Lock()
					createErrs = append(createErrs, fmt.Sprintf("pod %s: %v", podName, err))
					uidMutex.Unlock()
				}
			}(i)
		}
		wg.Wait()

		// Without these the phase below measures provisioning for however many Services happened to
		// be created, while still reporting the full count as a success.
		Expect(createErrs).To(BeEmpty(), "every Service and backend Pod must be created before timing provisioning")
		Expect(serviceUIDs).To(HaveLen(numServices),
			"all %d services must exist before measuring provisioning time", numServices)

		k8sCreateDuration := time.Since(createStart)
		utils.Logf("✓ K8s API calls completed in %v (%d services created)", k8sCreateDuration, len(serviceUIDs))

		// ============================================
		// PHASE 2: WAIT FOR ALL TO BE PROVISIONED IN AZURE
		// ============================================
		utils.Logf("\n--- PHASE 2: Waiting for Azure provisioning ---")
		provisionStart := time.Now()

		for {
			if time.Since(provisionStart) > 30*time.Minute {
				utils.Logf("WARNING: Timeout waiting for Azure provisioning")
				break
			}

			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}

			// Count provisioned
			uidMutex.Lock()
			for _, sgSvc := range sgResponse.Value {
				if _, exists := serviceUIDs[sgSvc.Name]; exists {
					serviceUIDs[sgSvc.Name] = true
				}
			}

			total := 0
			for _, provisioned := range serviceUIDs {
				if provisioned {
					total++
				}
			}
			uidMutex.Unlock()

			utils.Logf("  Provisioned: %d/%d", total, numServices)

			if total >= numServices {
				break
			}
			time.Sleep(5 * time.Second)
		}

		totalCreateDuration := time.Since(createStart)
		utils.Logf("✓ ALL %d SERVICES CREATED AND PROVISIONED IN: %v", numServices, totalCreateDuration)

		// ============================================
		// PHASE 3: DELETE ALL SERVICES IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 3: Deleting %d services in parallel ---", numServices)
		deleteStart := time.Now()

		var deleteWg sync.WaitGroup
		for i := 0; i < numServices; i++ {
			deleteWg.Add(1)
			go func(index int) {
				defer deleteWg.Done()
				cs.CoreV1().Services(ns.Name).Delete(context.TODO(), fmt.Sprintf("perf-svc-%d", index), metav1.DeleteOptions{})
			}(i)
		}
		deleteWg.Wait()

		k8sDeleteDuration := time.Since(deleteStart)
		utils.Logf("✓ K8s delete calls completed in %v", k8sDeleteDuration)

		// ============================================
		// PHASE 4: WAIT FOR ALL TO BE GONE FROM K8S
		// ============================================
		utils.Logf("\n--- PHASE 4: Waiting for services to disappear from K8s ---")

		for {
			if time.Since(deleteStart) > 20*time.Minute {
				utils.Logf("WARNING: Timeout waiting for deletion")
				break
			}

			remaining := 0
			for i := 0; i < numServices; i++ {
				_, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), fmt.Sprintf("perf-svc-%d", i), metav1.GetOptions{})
				if !errors.IsNotFound(err) {
					remaining++
				}
			}

			utils.Logf("  Remaining in K8s: %d", remaining)
			if remaining == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}

		totalDeleteDuration := time.Since(deleteStart)
		utils.Logf("✓ ALL %d SERVICES DELETED IN: %v", numServices, totalDeleteDuration)

		// ============================================
		// FINAL SUMMARY
		// ============================================
		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("PERFORMANCE TEST RESULTS - %d SERVICES", numServices)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("")
		utils.Logf("CREATION (K8s create → Azure provisioned):  %v", totalCreateDuration)
		utils.Logf("DELETION (K8s delete → Gone from K8s):      %v", totalDeleteDuration)
		utils.Logf("")
		utils.Logf("Creation rate: %.2f services/second", float64(numServices)/totalCreateDuration.Seconds())
		utils.Logf("Deletion rate: %.2f services/second", float64(numServices)/totalDeleteDuration.Seconds())
		utils.Logf(strings.Repeat("=", 80))

		// Cleanup namespace
		utils.DeleteNamespace(cs, ns.Name)

		Expect(totalCreateDuration).To(BeNumerically("<", 30*time.Minute), "Creation should complete within 30 minutes")
		Expect(totalDeleteDuration).To(BeNumerically("<", 20*time.Minute), "Deletion should complete within 20 minutes")
	})
})
