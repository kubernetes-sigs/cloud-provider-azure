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

package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// EnsureLoadBalancer returns an empty status immediately and the difftracker provisions the PIP
// asynchronously; updateServiceLoadBalancerStatus later resolves the Service by UID and backfills
// Service.Status.LoadBalancer.Ingress. This exercises that backfill end to end and confirms the
// patched IP matches the provisioned Public IP.
var _ = Describe("SLB - Status Backfill", Label(slbTestLabel), func() {
	basename := "slb-status-backfill-test"
	serviceName := "inbound-status-backfill"

	var (
		cs clientset.Interface
		ns *v1.Namespace
	)

	labels := map[string]string{"app": serviceName}

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
			if err != nil && strings.Contains(err.Error(), "timed out waiting for the condition") {
				utils.Logf("WARNING: Namespace deletion timed out; cleanup will complete asynchronously")
			} else {
				Expect(err).NotTo(HaveOccurred())
			}

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

	It("backfills Service status ingress with the provisioned Public IP", func() {
		By("Creating a LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              labels,
				Ports: []v1.ServicePort{
					{
						Port:       5000,
						TargetPort: intstr.FromInt(30154),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)
		utils.Logf("Created service %s/%s (UID %s)", ns.Name, serviceName, serviceUID)

		defer func() {
			By("Cleaning up service")
			Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})).To(Succeed())
		}()

		By("Waiting for Azure to provision LoadBalancer resources")
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, -1)
		}, 3*time.Minute, 10*time.Second).Should(Succeed(),
			"service should be reconciled in Azure and the Service Gateway")

		By("Verifying the Service status is backfilled with the provisioned Public IP")
		Eventually(func() error {
			externalIP, err := getServicePublicIPAddress(serviceUID)
			if err != nil {
				return err
			}
			if externalIP == "" {
				return fmt.Errorf("public IP %s-pip not yet provisioned", serviceUID)
			}
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if len(svc.Status.LoadBalancer.Ingress) == 0 || svc.Status.LoadBalancer.Ingress[0].IP == "" {
				return fmt.Errorf("service status not yet backfilled with an ingress IP")
			}
			if got := svc.Status.LoadBalancer.Ingress[0].IP; got != externalIP {
				return fmt.Errorf("service status ingress IP %q does not match provisioned Public IP %q", got, externalIP)
			}
			utils.Logf("✓ Service status backfilled with Public IP %s", externalIP)
			return nil
		}, 3*time.Minute, 10*time.Second).Should(Succeed(),
			"Service.Status.LoadBalancer.Ingress must be backfilled with the provisioned Public IP")
	})
})

// getServicePublicIPAddress returns the IP address of the Public IP that the difftracker
// provisions for a service (named "<uid>-pip"), or an empty string if it does not yet exist.
func getServicePublicIPAddress(serviceUID string) (string, error) {
	expectedPIPName := fmt.Sprintf("%s-pip", serviceUID)
	out, err := exec.Command("az", "network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("list public IPs: %w (%s)", err, string(out))
	}
	var pips []AzurePublicIP
	if err := json.Unmarshal(out, &pips); err != nil {
		return "", fmt.Errorf("parse public IP list: %w", err)
	}
	for i := range pips {
		if pips[i].Name == expectedPIPName {
			return pips[i].IPAddress, nil
		}
	}
	return "", nil
}
