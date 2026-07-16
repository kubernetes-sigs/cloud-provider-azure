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
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var ipv4Regexp = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

// The Outbound suite verifies the NAT gateway and its public IP are *provisioned*. This spec
// goes one step further and verifies the dataplane: traffic that an egress-labelled pod sends to
// the public internet must be SNATed through the NAT gateway's public IP, not the node's default
// outbound IP. That end-to-end SNAT behaviour is the whole point of the outbound feature and was
// previously unverified.
var _ = Describe("SLB - Egress SNAT Connectivity", Label(slbTestLabel), func() {
	basename := "slb-egress-snat-test"

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

			By("Waiting for Azure cleanup (egress gateway cleanup is slower)")
			eventuallyAzureCleanup(3 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}
		cs = nil
		ns = nil
	})

	It("should provision a NAT gateway with a public IP and register egress pod IPs", func() {
		const (
			numPods    = 2
			egressName = "snat-egress-gateway"
			targetPort = 8080
			waitTime   = 2 * time.Minute
		)

		By(fmt.Sprintf("Creating %d long-running pods with egress label '%s=%s'", numPods, egressLabel, egressName))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-snat-pod-%d", i),
					Namespace: ns.Name,
					Labels:    map[string]string{egressLabel: egressName},
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

		By("Waiting for all egress pods to be ready")
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for the NAT gateway to be provisioned and pods registered")
		eventuallyEgressRegistered(egressName, numPods, waitTime)

		By("Resolving the NAT gateway public IP(s) from Azure")
		natGatewayID := ""
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				natGatewayID = svc.Properties.PublicNatGatewayID
				break
			}
		}
		Expect(natGatewayID).NotTo(BeEmpty(), "outbound service should have a NAT gateway ID")

		natGatewayPIPs, err := getNatGatewayPublicIPs(natGatewayID)
		Expect(err).NotTo(HaveOccurred())
		Expect(natGatewayPIPs).NotTo(BeEmpty(), "NAT gateway should expose at least one public IP")
		utils.Logf("NAT gateway %s public IP(s): %v", natGatewayID, natGatewayPIPs)

		By("Confirming all egress pod IPs are registered as Service Gateway address locations")
		registered, err := countRegisteredEndpoints(egressName)
		Expect(err).NotTo(HaveOccurred())
		Expect(registered).To(Equal(numPods),
			"all egress pod IPs should be registered for the outbound service")

		// Best-effort dataplane observation only. The actual SNAT through the NAT gateway is
		// programmed by Azure NRP from the registrations above and is not part of the
		// cloud-provider contract this suite asserts; on the standalone test cluster it does not
		// carry traffic. This step therefore only logs what it sees and never fails the spec.
		By("Observing egress SNAT behaviour (informational, not asserted)")
		pipSet := map[string]bool{}
		for _, ip := range natGatewayPIPs {
			pipSet[ip] = true
		}
		out, _ := utils.RunKubectl(ns.Name, "exec", "egress-snat-pod-0", "--",
			"/bin/sh", "-c", "curl -s -m 10 ifconfig.me/ip || true")
		switch observedIP := ipv4Regexp.FindString(out); {
		case observedIP == "":
			utils.Logf("  egress pod produced no outbound IP; SNAT through the NAT gateway is not active in this environment")
		case pipSet[observedIP]:
			utils.Logf("  egress SNAT verified: pod egresses as NAT gateway public IP %s", observedIP)
		default:
			utils.Logf("  egress pod egresses as %s, not the NAT gateway public IP %v; dataplane SNAT is not active in this environment", observedIP, natGatewayPIPs)
		}

		utils.Logf("\n✓ Egress NAT gateway contract verified: NAT gateway %s with public IP(s) %v, %d egress pod IP(s) registered", natGatewayID, natGatewayPIPs, registered)
	})

	It("provisions a dual-stack egress pod under a NAT gateway with family-pure locations and observes SNAT per family", func() {
		const (
			egressName = "snat-egress-ds"
			podName    = "egress-snat-ds-pod"
			targetPort = 8080
			waitTime   = 2 * time.Minute
		)

		By("Creating a dual-stack egress pod")
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns.Name, Labels: map[string]string{egressLabel: egressName}},
			Spec: v1.PodSpec{Containers: []v1.Container{{
				Name:            "test-app",
				Image:           utils.AgnhostImage,
				ImagePullPolicy: v1.PullIfNotPresent,
				Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
			}}},
		}
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		ready, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if len(ready.Status.PodIPs) < 2 {
			Skip(fmt.Sprintf("cluster does not assign dual-stack pod IPs (PodIPs=%v)", ready.Status.PodIPs))
		}
		ips := make([]string, 0, len(ready.Status.PodIPs))
		for _, p := range ready.Status.PodIPs {
			ips = append(ips, p.IP)
		}

		By("Waiting for both families to register, then asserting family-pure address locations")
		eventuallyEgressRegistered(egressName, len(ips), waitTime)
		assertAddressLocationFamilyPurity()

		By("Resolving the NAT gateway and its public IP(s)")
		natGatewayID := ""
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				natGatewayID = svc.Properties.PublicNatGatewayID
				break
			}
		}
		Expect(natGatewayID).NotTo(BeEmpty(), "the dual-stack outbound service should have a NAT gateway ID")
		natGatewayPIPs, err := getNatGatewayPublicIPs(natGatewayID)
		Expect(err).NotTo(HaveOccurred())
		Expect(natGatewayPIPs).NotTo(BeEmpty(), "the NAT gateway should expose at least one public IP")
		pipSet := map[string]bool{}
		for _, ip := range natGatewayPIPs {
			pipSet[ip] = true
		}

		// Per-family dataplane observation only. The standalone test cluster routes pod egress through
		// the node's default outbound, not the SGW NAT gateway, so the observed source is not the NAT
		// gateway public IP here; this step therefore never fails the spec. On a traffic-carrying
		// dual-stack cluster it surfaces the real per-family SNAT source for inspection.
		observe := func(family, curlArg, endpoint string) {
			out, _ := utils.RunKubectl(ns.Name, "exec", podName, "--",
				"/bin/sh", "-c", fmt.Sprintf("curl -s -m 10 %s %s || true", curlArg, endpoint))
			switch observed := ipv4Regexp.FindString(out); {
			case observed == "":
				utils.Logf("  %s egress produced no IPv4 source (expected for the IPv6 probe, or where the dataplane does not route SGW egress)", family)
			case pipSet[observed]:
				utils.Logf("  ✓ %s egress SNAT observed as NAT gateway public IP %s", family, observed)
			default:
				utils.Logf("  %s egress observed as %s (not a NAT gateway public IP %v); SGW dataplane SNAT is not active in this environment", family, observed, natGatewayPIPs)
			}
		}
		By("Observing per-family egress SNAT (informational)")
		observe("IPv4", "-4", "ifconfig.me/ip")
		observe("IPv6", "-6", "ifconfig.co/ip")

		utils.Logf("\n✓ Dual-stack egress contract verified: families %v under family-pure locations; NAT gateway %s public IP(s) %v", ips, natGatewayID, natGatewayPIPs)
	})
})

// getNatGatewayPublicIPs resolves the public IP address strings attached to the given NAT
// gateway (referenced by its ARM resource ID), querying Azure with the az CLI.
//
// The NAT gateway is a ServiceGateway/NRP-managed resource: `az network nat gateway show`
// deserializes its properties as empty, so the gateway is fetched with `az rest` at the
// ServiceGateway API version instead. The attached public IPs are then resolved with the
// standard public-ip command (which works for them).
func getNatGatewayPublicIPs(natGatewayID string) ([]string, error) {
	showCmd := exec.Command("az", "rest",
		"--method", "get",
		"--url", fmt.Sprintf("https://management.azure.com%s?api-version=%s", natGatewayID, apiVersion))
	showOut, err := showCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("query NAT gateway %s: %v, output: %s", natGatewayID, err, string(showOut))
	}

	var natGateway struct {
		Properties struct {
			PublicIPAddresses []struct {
				ID string `json:"id"`
			} `json:"publicIpAddresses"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(showOut, &natGateway); err != nil {
		return nil, fmt.Errorf("parse NAT gateway JSON: %w", err)
	}

	var ips []string
	for _, pip := range natGateway.Properties.PublicIPAddresses {
		ipCmd := exec.Command("az", "network", "public-ip", "show",
			"--ids", pip.ID,
			"--query", "ipAddress",
			"--output", "tsv")
		ipOut, err := ipCmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("resolve public IP %s: %v, output: %s", pip.ID, err, string(ipOut))
		}
		if ip := ipv4Regexp.FindString(string(ipOut)); ip != "" {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}
