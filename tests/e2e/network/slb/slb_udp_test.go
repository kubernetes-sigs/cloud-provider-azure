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
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// udpLBRule captures the LB rule fields needed to validate a UDP service: the protocol and
// whether TCP reset is enabled. EnableTCPReset is a pointer so we can tell "unset" from
// "false".
type udpLBRule struct {
	Name           string `json:"name"`
	FrontendPort   int32  `json:"frontendPort"`
	BackendPort    int32  `json:"backendPort"`
	Protocol       string `json:"protocol"`
	EnableTCPReset *bool  `json:"enableTcpReset"`
}

// getLoadBalancerRules returns all load-balancing rules on the LB named serviceUID.
func getLoadBalancerRules(serviceUID string) ([]udpLBRule, error) {
	cmd := exec.Command("az", "network", "lb", "rule", "list",
		"--resource-group", resourceGroupName,
		"--lb-name", serviceUID,
		"--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list LB rules for %s: %w, output: %s", serviceUID, err, string(output))
	}
	var rules []udpLBRule
	if err := json.Unmarshal(output, &rules); err != nil {
		return nil, fmt.Errorf("failed to parse LB rules JSON: %w", err)
	}
	return rules, nil
}

// A UDP Container Load Balancer service must produce a UDP load-balancing rule. Crucially,
// the rule must NOT enable TCP reset: Azure rejects an LB PUT that sets EnableTcpReset on a
// UDP rule, so the difftracker only sets it for TCP. (If that gating regressed, the LB PUT
// would fail and the service would never reconcile.)
var _ = Describe("SLB - UDP Service", Label(slbTestLabel), func() {
	basename := "slb-udp-test"

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
			eventuallyAzureCleanup(3 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	It("should create a UDP load-balancing rule without TCP reset", func() {
		const (
			serviceName = "udp-service"
			numPods     = 3
			httpPort    = 8080
			udpPort     = 8053
			servicePort = int32(53)
		)
		labels := map[string]string{"app": serviceName}

		By("Creating backend pods that serve UDP (and HTTP for readiness)")
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "test-app",
						Image:           utils.AgnhostImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", httpPort), fmt.Sprintf("--udp-port=%d", udpPort)},
						ReadinessProbe: &v1.Probe{
							ProbeHandler: v1.ProbeHandler{
								HTTPGet: &v1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt(httpPort),
								},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       2,
						},
					}},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a UDP LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports: []v1.ServicePort{
					{Name: "dns", Port: servicePort, TargetPort: intstr.FromInt(udpPort), Protocol: v1.ProtocolUDP},
				},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("UDP service created with UID=%s", serviceUID)

		By("Waiting for the UDP service to be reconciled with all pods registered")
		// A successful reconcile already proves Azure accepted the UDP LB rule, which it would
		// not if TCP reset were (incorrectly) enabled on a UDP rule.
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, numPods)
		}, 90*time.Second, 10*time.Second).Should(Succeed(),
			"UDP service should be reconciled in Azure and the Service Gateway")

		By("Verifying the LB rule is UDP and does not enable TCP reset")
		rules, err := getLoadBalancerRules(serviceUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rules).To(HaveLen(1), "UDP service should have exactly one LB rule")
		rule := rules[0]
		Expect(rule.Protocol).To(Equal("Udp"), "LB rule protocol should be UDP")
		Expect(rule.FrontendPort).To(Equal(servicePort), "LB rule frontend port should match the service port")
		if rule.EnableTCPReset != nil {
			Expect(*rule.EnableTCPReset).To(BeFalse(), "TCP reset must not be enabled on a UDP rule")
		}

		utils.Logf("✓ UDP service provisioned a UDP LB rule (TCP reset disabled) with %d registered endpoints", numPods)
	})
})
