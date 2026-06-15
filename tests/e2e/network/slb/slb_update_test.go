/*
Copyright 2026 The Kubernetes Authors.

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

// Package network: this file exercises the Engine.UpdateService path end-to-end:
//   - port edits go through UpdateService -> StateUpdateInProgress -> updateInboundService
//   - the LB ARM resource is re-PUT in place (same name == same UID, no recreation)
//   - the SGW Service registration stays stable across port edits (LB backend pool ID
//     is the same), so SGW state should not flap
//   - rapid consecutive updates converge on the latest desired config (drift handling)
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// azureLBRule is the minimal projection of `az network lb rule list` JSON we need.
type azureLBRule struct {
	Name         string `json:"name"`
	FrontendPort int32  `json:"frontendPort"`
	BackendPort  int32  `json:"backendPort"`
	Protocol     string `json:"protocol"`
}

// getLoadBalancerFrontendPorts reads all frontend ports configured on the LB
// whose name == serviceUID. Returns sorted list for deterministic comparison.
func getLoadBalancerFrontendPorts(serviceUID string) ([]int32, error) {
	cmd := exec.Command("az", "network", "lb", "rule", "list",
		"--resource-group", resourceGroupName,
		"--lb-name", serviceUID,
		"--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list LB rules for %s: %w, output: %s", serviceUID, err, string(output))
	}
	var rules []azureLBRule
	if err := json.Unmarshal(output, &rules); err != nil {
		return nil, fmt.Errorf("failed to parse LB rules JSON: %w", err)
	}
	ports := make([]int32, 0, len(rules))
	for _, r := range rules {
		ports = append(ports, r.FrontendPort)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
}

// getServiceGatewayBackendPoolID returns the LB backend pool ID registered for serviceUID
// in the SGW. Used to confirm the SGW Service entry is stable across port-only updates.
func getServiceGatewayBackendPoolID(serviceUID string) (string, error) {
	resp, err := queryServiceGatewayServices()
	if err != nil {
		return "", err
	}
	for _, s := range resp.Value {
		if s.Name == serviceUID {
			if len(s.Properties.LoadBalancerBackendPools) == 0 {
				return "", fmt.Errorf("service %s in SGW has no backend pools", serviceUID)
			}
			return s.Properties.LoadBalancerBackendPools[0].ID, nil
		}
	}
	return "", fmt.Errorf("service %s not found in SGW", serviceUID)
}

// getServiceGatewayServiceEtag returns the etag of the SGW Service entry for serviceUID.
// Etag changes on every Azure write, so a stable etag is a strong "no PUT happened" witness.
func getServiceGatewayServiceEtag(serviceUID string) (string, error) {
	resp, err := queryServiceGatewayServices()
	if err != nil {
		return "", err
	}
	for _, s := range resp.Value {
		if s.Name == serviceUID {
			return s.Etag, nil
		}
	}
	return "", fmt.Errorf("service %s not found in SGW", serviceUID)
}

var _ = Describe("Container Load Balancer Update", Label(slbTestLabel), func() {
	basename := "slb-update-test"
	serviceName := "update-service"

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
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	// Single port update: create svc on port A, change to port B. Verifies that
	// 1) LB ARM resource is re-PUT (same name/UID, no recreation),
	// 2) the LB rule's frontend port now equals port B,
	// 3) the SGW Service entry's backend-pool ID is unchanged (stable across port edits).
	It("should propagate a port edit to the Azure LB rule via UpdateService", func() {
		const (
			numPods     = 5
			initialPort = int32(80)
			updatedPort = int32(8080)
			targetPort  = 8080
			provision   = 60 * time.Second
			updateWait  = 45 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating service with port=%d", initialPort))
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{{
					Port:       initialPort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for Azure provisioning")
		time.Sleep(provision)

		By("Verifying initial Azure resources exist")
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By(fmt.Sprintf("Verifying LB rule frontend port == %d initially", initialPort))
		ports, err := getLoadBalancerFrontendPorts(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(ports).To(Equal([]int32{initialPort}),
			"LB should have exactly one rule on the initial port before update")

		By("Recording SGW backend-pool ID before the update")
		backendPoolBefore, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("SGW backend pool BEFORE update: %s", backendPoolBefore)

		By(fmt.Sprintf("Updating service port from %d to %d (UpdateService path)", initialPort, updatedPort))
		retrieved, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		retrieved.Spec.Ports[0].Port = updatedPort
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrieved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure to apply the update")
		time.Sleep(updateWait)

		By("Verifying the LB still exists under the SAME UID (no recreation)")
		// verifyAzureResources looks up LB by name == serviceUID; if the engine had
		// recreated the LB under a different name, this would fail.
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By(fmt.Sprintf("Verifying LB rule frontend port now == %d", updatedPort))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 90*time.Second, 5*time.Second).Should(Equal([]int32{updatedPort}),
			"LB rule frontend port should have been re-PUT to the new value")

		By("Verifying SGW backend-pool ID is unchanged (stable across port edits)")
		backendPoolAfter, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(backendPoolAfter).To(Equal(backendPoolBefore),
			"SGW Service entry's backend pool ID must not change for a port-only update")

		utils.Logf("\n✓ Single port update test passed: LB rule %d → %d (UID preserved, SGW stable)",
			initialPort, updatedPort)
	})

	// Rapid consecutive updates: A -> B -> C in quick succession. The engine's drift
	// detection (see OnServiceCreationComplete -> configsEqualForUpdate) must converge
	// on the LATEST desired state regardless of how many in-flight updates happened.
	It("should converge to the latest desired port across rapid consecutive updates", func() {
		const (
			numPods    = 3
			portA      = int32(80)
			portB      = int32(8080)
			portC      = int32(443)
			targetPort = 8080
			provision  = 60 * time.Second
			updateWait = 60 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating service with port=%d", portA))
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{{
					Port:       portA,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for initial Azure provisioning")
		time.Sleep(provision)
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By(fmt.Sprintf("Issuing rapid edits: %d -> %d -> %d", portA, portB, portC))
		for _, p := range []int32{portB, portC} {
			r, gerr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			Expect(gerr).NotTo(HaveOccurred())
			r.Spec.Ports[0].Port = p
			_, uerr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), r, metav1.UpdateOptions{})
			Expect(uerr).NotTo(HaveOccurred())
			utils.Logf("Issued port update -> %d", p)
		}

		By(fmt.Sprintf("Waiting for engine to converge on final port %d", portC))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, updateWait+30*time.Second, 5*time.Second).Should(Equal([]int32{portC}),
			"final LB rule frontend port must equal the LATEST desired (drift handling)")

		By("Verifying LB UID unchanged (single resource, no recreation)")
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		utils.Logf("\n✓ Rapid update convergence test passed: %d -> %d -> %d => final %d",
			portA, portB, portC, portC)
	})

	// Multi-port add/remove: a single LoadBalancer service goes through ports
	//   [80]  ->  [80, 443]  ->  [443]
	// Verifies that the LB rule **set** (not just a single rule) is reconciled correctly
	// on each Update — adding a port creates a new rule, removing a port deletes one,
	// and the LB ARM resource is re-PUT (UID preserved) on each transition.
	It("should reconcile the LB rule set when ports are added and removed", func() {
		const (
			numPods    = 4
			portA      = int32(80)
			portB      = int32(443)
			targetPort = 8080
			provision  = 60 * time.Second
			updateWait = 75 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating service with single port=[%d]", portA))
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{{
					Name:       "http",
					Port:       portA,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for initial Azure provisioning")
		time.Sleep(provision)
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By(fmt.Sprintf("Verifying LB rule set == [%d]", portA))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 60*time.Second, 5*time.Second).Should(Equal([]int32{portA}))

		backendPoolBefore, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		// --- ADD port ---
		By(fmt.Sprintf("Adding port %d to the service (now [%d, %d])", portB, portA, portB))
		retrieved, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		retrieved.Spec.Ports = []v1.ServicePort{
			{Name: "http", Port: portA, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP},
			{Name: "https", Port: portB, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP},
		}
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrieved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Verifying LB rule set converges on [%d, %d]", portA, portB))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, updateWait, 5*time.Second).Should(Equal([]int32{portA, portB}),
			"adding a port must create a new LB rule without removing the existing one")

		By("Verifying LB UID still preserved after add")
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		// --- REMOVE port ---
		By(fmt.Sprintf("Removing port %d from the service (now [%d])", portA, portB))
		retrieved, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		retrieved.Spec.Ports = []v1.ServicePort{
			{Name: "https", Port: portB, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP},
		}
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrieved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Verifying LB rule set converges on [%d]", portB))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, updateWait, 5*time.Second).Should(Equal([]int32{portB}),
			"removing a port must delete the corresponding LB rule")

		By("Verifying LB UID still preserved after remove")
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By("Verifying SGW backend-pool ID stable across the entire add/remove sequence")
		backendPoolAfter, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(backendPoolAfter).To(Equal(backendPoolBefore),
			"SGW Service entry's backend pool ID must not change for port-only edits")

		utils.Logf("\n✓ Multi-port reconciliation test passed: [%d] → [%d,%d] → [%d] (UID preserved, SGW stable)",
			portA, portA, portB, portB)
	})

	// Idempotent no-op: after the LB is created and LastAppliedConfig is populated,
	// we issue a follow-up Update on the Service that does NOT change anything the
	// engine cares about (port/protocol/targetPort identical). The CCM watch event
	// fires and calls Engine.UpdateService, which must short-circuit on the
	// LastAppliedConfig.Equals check — no LB PUT and no SGW write.
	//
	// Witness: the SGW Service entry's etag changes on every Azure write. If the etag
	// stays the same across the no-op Update, the engine truly didn't churn.
	It("should be a no-op when the Service spec has not changed", func() {
		const (
			numPods    = 3
			port       = int32(80)
			targetPort = 8080
			provision  = 60 * time.Second
			settle     = 30 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating service with port=%d", port))
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{{
					Port:       port,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for Azure provisioning to populate LastAppliedConfig")
		time.Sleep(provision)
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By("Recording SGW Service etag and LB rule set BEFORE the no-op Update")
		etagBefore, err := getServiceGatewayServiceEtag(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(etagBefore).NotTo(BeEmpty(), "SGW Service entry must have an etag")
		utils.Logf("SGW Service etag BEFORE: %s", etagBefore)

		portsBefore, err := getLoadBalancerFrontendPorts(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(portsBefore).To(Equal([]int32{port}))

		// Force a Service Update with an annotation bump (no spec change).
		// This bumps resourceVersion → CCM reconciler fires → Engine.UpdateService(config)
		// is called with config equal to LastAppliedConfig → must short-circuit.
		By("Bumping a benign annotation to force a Service reconcile (no spec change)")
		retrieved, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if retrieved.Annotations == nil {
			retrieved.Annotations = map[string]string{}
		}
		retrieved.Annotations["e2e-noop-trigger"] = fmt.Sprintf("%d", time.Now().UnixNano())
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrieved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for any (incorrect) reconcile churn to settle")
		time.Sleep(settle)

		By("Verifying SGW Service etag is unchanged (engine did not write to SGW)")
		etagAfter, err := getServiceGatewayServiceEtag(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(etagAfter).To(Equal(etagBefore),
			"SGW etag must be stable when the LB config is unchanged — etag drift implies a spurious PUT")

		By("Verifying LB rule set is unchanged (engine did not re-PUT the LB)")
		portsAfter, err := getLoadBalancerFrontendPorts(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(portsAfter).To(Equal(portsBefore),
			"LB rule set must be unchanged across a no-op Service Update")

		utils.Logf("\n✓ Idempotent no-op test passed: SGW etag stable, LB rule set unchanged")
	})

	// Endpoint churn during update: simultaneously
	//   - issue a port edit (UpdateService path → StateUpdateInProgress)
	//   - scale the backend pod set up (UpdateEndpoints path → triggers locations updater)
	// Both paths share the engine's pendingServiceOps lock and the SGW state. This spec
	// proves they don't deadlock or stomp each other: the final LB rule must reflect the
	// new port AND the service must remain healthy with the larger backing set.
	It("should handle a port edit and an endpoint scale-up concurrently", func() {
		const (
			initialPods = 3
			extraPods   = 5
			portBefore  = int32(80)
			portAfter   = int32(8080)
			targetPort  = 8080
			provision   = 60 * time.Second
			settle      = 90 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating initial %d pods", initialPods))
		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By(fmt.Sprintf("Creating service with port=%d", portBefore))
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{{
					Port:       portBefore,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Service created with UID=%s", serviceUID)

		By("Waiting for initial Azure provisioning")
		time.Sleep(provision)
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		backendPoolBefore, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		// Burst: issue the port edit and the scale-up back to back. These trigger
		// the UpdateService path and the UpdateEndpoints path on the same service.
		By(fmt.Sprintf("Issuing concurrent port edit %d→%d AND scaling pods %d→%d",
			portBefore, portAfter, initialPods, initialPods+extraPods))

		retrieved, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		retrieved.Spec.Ports[0].Port = portAfter
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrieved, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Issued port update -> %d", portAfter)

		for i := initialPods; i < initialPods+extraPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d additional pods", extraPods)

		By("Waiting for all pods to become Ready (endpoint set settles)")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for engine to converge on the final LB+endpoint state")
		time.Sleep(settle)

		By(fmt.Sprintf("Verifying LB rule converges on [%d] (port edit applied)", portAfter))
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, 90*time.Second, 5*time.Second).Should(Equal([]int32{portAfter}),
			"port edit must complete despite concurrent endpoint churn")

		By("Verifying LB UID still preserved (no recreation)")
		Expect(verifyAzureResources(serviceUID)).To(Succeed())

		By("Verifying SGW backend-pool ID stable")
		backendPoolAfter, err := getServiceGatewayBackendPoolID(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(backendPoolAfter).To(Equal(backendPoolBefore),
			"SGW Service entry must remain stable across concurrent UpdateService+UpdateEndpoints")

		By("Verifying the service has an external IP (svc is healthy after the burst)")
		Eventually(func() bool {
			s, gerr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if gerr != nil {
				return false
			}
			return len(s.Status.LoadBalancer.Ingress) > 0 && s.Status.LoadBalancer.Ingress[0].IP != ""
		}, 60*time.Second, 5*time.Second).Should(BeTrue())

		utils.Logf("\n✓ Concurrent update+endpoint-churn test passed: port=%d, %d backing pods, UID stable",
			portAfter, initialPods+extraPods)
	})
})
