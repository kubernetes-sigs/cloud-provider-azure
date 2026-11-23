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

package servicegateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

const (
	natGatewayDriftLabel          = "SLB-NatGwDrift"
	nrpKnownIssueLabel            = "nrp-known-issue"
	ccmKnownGapLabel              = "ccm-known-gap"
	destructiveLabel              = "destructive"
	runKnownIssuesEnv             = "SGW_E2E_RUN_KNOWN_ISSUES"
	runDestructiveDefaultNATEnv   = "SGW_E2E_RUN_DESTRUCTIVE_DEFAULT_NAT"
	natGatewayDriftTestTag        = "sgw-natgw-drift-test"
	driftDataplaneSkipMessage     = "egress pod produced no outbound IP; this environment does not carry egress dataplane traffic, so SNAT through the NAT gateway cannot be asserted"
	natGatewayDriftPodName        = "egress-natgw-drift-pod"
	natGatewayDriftTargetPort     = 8080
	natGatewayDriftProvisionWait  = 5 * time.Minute
	natGatewayDriftStableWindow   = 2 * time.Minute
	natGatewayDriftKnownBugWindow = 3 * time.Minute
	natGatewayDriftRepairWait     = 10 * time.Minute
	natGatewayDriftCleanupWait    = 6 * time.Minute
	natGatewayDriftPollInterval   = 10 * time.Second
	defaultOutboundServiceName    = "default-natgw"
	natGatewayDriftMaxEmptyProbes = 3
)

var errNoEgressSource = errors.New("produced no observable outbound IPv4 source")

type natGatewayDriftCase struct {
	egressName string
	podName    string
	service    ServiceGatewayService
	nat        natGatewayDriftResource
	natBody    map[string]any
	pips       []string
	dataplane  bool
}

// egressErr checks pod egress only when the environment carries egress traffic.
func (tc *natGatewayDriftCase) egressErr(namespace string, pips []string) error {
	if !tc.dataplane {
		return nil
	}
	return podEgressesFromPIPsErr(namespace, tc.podName, pips)
}

type armIDReference struct {
	ID string `json:"id"`
}

type natGatewayDriftResource struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		ProvisioningState    string           `json:"provisioningState"`
		ResourceGUID         string           `json:"resourceGuid"`
		IdleTimeoutInMinutes *int             `json:"idleTimeoutInMinutes,omitempty"`
		PublicIPAddresses    []armIDReference `json:"publicIpAddresses"`
		PublicIPAddressesV6  []armIDReference `json:"publicIpAddressesV6"`
		ServiceGateway       *armIDReference  `json:"serviceGateway,omitempty"`
	} `json:"properties"`
}

type publicIPDriftResource struct {
	ID         string `json:"id"`
	Location   string `json:"location"`
	Properties struct {
		IPAddress         string `json:"ipAddress"`
		ProvisioningState string `json:"provisioningState"`
	} `json:"properties"`
}

var _ = Describe("SLB - NAT Gateway Drift", Label(slbTestLabel, natGatewayDriftLabel), func() {
	const basename = "slb-natgw-drift"

	var (
		cs                 clientset.Interface
		ns                 *v1.Namespace
		egressNames        []string
		extraPublicIPNames []string
		natRestores        []func() error
	)

	BeforeEach(func() {
		cs = nil
		ns = nil
		egressNames = nil
		extraPublicIPNames = nil
		natRestores = nil
	})

	AfterEach(func() {
		var cleanupErrs []error
		for _, restore := range natRestores {
			By("Restoring out-of-band NAT gateway drift")
			if err := restore(); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if cs != nil && ns != nil {
			if err := utils.DeleteNamespace(cs, ns.Name); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if len(egressNames) > 0 {
			By("Verifying egress identity NAT Gateway teardown")
			if err := pollUntilSucceeds(natGatewayDriftCleanupWait, func() error {
				return natGatewayCleanupErr(egressNames)
			}); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		for _, pipName := range extraPublicIPNames {
			By(fmt.Sprintf("Deleting extra Public IP %s", pipName))
			if err := pollUntilSucceeds(natGatewayDriftCleanupWait, func() error {
				return deletePublicIPNamedErr(pipName)
			}); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		Expect(cleanupErrs).To(BeEmpty(), "NAT gateway drift cleanup failed")
	})

	createNamespace := func() {
		var err error
		ensureSLBConfigInitialized()
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	}

	registerNATRestore := func(natGatewayID string, body map[string]any, verify func() error) {
		original := cloneJSONMap(body)
		restore := func() error {
			if _, _, err := putNatGateway(natGatewayID, original); err != nil {
				return err
			}
			if verify == nil {
				return nil
			}
			return pollUntilSucceeds(natGatewayDriftRepairWait, verify)
		}
		natRestores = append(natRestores, restore)
	}

	setupEgressCase := func() *natGatewayDriftCase {
		createNamespace()
		egressName := randomNatGatewayDriftName()
		egressNames = append(egressNames, egressName)

		By(fmt.Sprintf("Creating one egress pod for identity %s", egressName))
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), driftEgressPod(ns.Name, natGatewayDriftPodName, egressName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Waiting for the NAT gateway, Public IP and outbound service registration")
		Eventually(func() error {
			want, err := livePodIPsWithLabel(cs, ns.Name, egressLabel, egressName)
			if err != nil {
				return err
			}
			if len(want) == 0 {
				return fmt.Errorf("egress pod has no live pod IPs yet")
			}
			return egressRegisteredMatchErr(egressName, want)
		}, natGatewayDriftProvisionWait, natGatewayDriftPollInterval).Should(Succeed())

		service, err := outboundServiceFor(egressName)
		Expect(err).NotTo(HaveOccurred())
		nat, natBody, err := getNatGateway(service.Properties.PublicNatGatewayID)
		Expect(err).NotTo(HaveOccurred())
		Expect(nat.Properties.ServiceGateway).NotTo(BeNil(), "identity NAT gateway must be linked to the ServiceGateway")

		var pips []string
		Eventually(func() error {
			var err error
			pips, err = getNatGatewayPublicIPs(nat.ID)
			if err != nil {
				return err
			}
			if len(pips) == 0 {
				return fmt.Errorf("identity NAT gateway %s has no resolved IPv4 Public IP yet", nat.ID)
			}
			return nil
		}, natGatewayDriftProvisionWait, natGatewayDriftPollInterval).Should(Succeed())

		By("Verifying the pod egresses as the identity NAT gateway Public IP")
		dataplane := podEgressObservable(ns.Name, natGatewayDriftPodName, pips)
		if !dataplane {
			AddReportEntry("egress not asserted", driftDataplaneSkipMessage+"; only control-plane assertions run")
		}

		return &natGatewayDriftCase{
			egressName: egressName,
			podName:    natGatewayDriftPodName,
			service:    service,
			nat:        nat,
			natBody:    natBody,
			pips:       sortedStrings(pips),
			dataplane:  dataplane,
		}
	}

	It("keeps a NAT gateway tag added out of band without recreating the NAT gateway", func() {
		tc := setupEgressCase()
		originalGUID := tc.nat.Properties.ResourceGUID
		Expect(originalGUID).NotTo(BeEmpty(), "resourceGuid is required to prove the NAT gateway was not recreated")

		By("Adding a user tag with an out-of-band NAT gateway PUT")
		body := cloneJSONMap(tc.natBody)
		tags := jsonObject(body, "tags")
		tags[natGatewayDriftTestTag] = tc.egressName
		body["tags"] = tags
		updated, _, err := putNatGateway(tc.nat.ID, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Properties.ProvisioningState).To(Equal("Succeeded"))
		for k, v := range tc.nat.Tags {
			Expect(updated.Tags).To(HaveKeyWithValue(k, v))
		}
		Expect(updated.Tags).To(HaveKeyWithValue(natGatewayDriftTestTag, tc.egressName))

		natKeepsUserChange := func() error {
			current, _, err := getNatGateway(tc.nat.ID)
			if err != nil {
				return err
			}
			if current.Tags[natGatewayDriftTestTag] != tc.egressName {
				return fmt.Errorf("NAT gateway lost the user tag %s", natGatewayDriftTestTag)
			}
			if current.Properties.ResourceGUID != originalGUID {
				return fmt.Errorf("NAT gateway was recreated: resourceGuid %s -> %s", originalGUID, current.Properties.ResourceGUID)
			}
			currentPIPs, err := getNatGatewayPublicIPs(current.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(sortedStrings(currentPIPs), tc.pips) {
				return fmt.Errorf("NAT gateway Public IPs changed: %v -> %v", tc.pips, currentPIPs)
			}
			if !reflect.DeepEqual(natPublicIPIDs(current), natPublicIPIDs(tc.nat)) {
				return fmt.Errorf("NAT gateway Public IP references changed: %v -> %v", natPublicIPIDs(tc.nat), natPublicIPIDs(current))
			}
			return tc.egressErr(ns.Name, tc.pips)
		}

		By("Verifying the NAT gateway keeps the user tag, resourceGuid and Public IPs (and egress when observable)")
		expectEgressHolds(natGatewayDriftStableWindow, natKeepsUserChange)
		afterService, err := outboundServiceFor(tc.egressName)
		Expect(err).NotTo(HaveOccurred())
		Expect(afterService).To(Equal(tc.service), "the SGW outbound service should not change for a tag-only NAT gateway PUT")

		if !IsCCMClusterConfigured() {
			utils.Logf("Skipping CCM restart portion: %s is not set", CCMKubeconfigEnvVar)
			return
		}

		By("Restarting the cloud-controller-manager")
		ccmClient, err := NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(ccmClient.CrashCCMAndWaitForRecovery(context.TODO(), CCMRecoveryTimeout)).To(Succeed())

		By("Verifying the tag and existing resources survive startup reconciliation")
		expectEgressHolds(natGatewayDriftStableWindow, natKeepsUserChange)
	})

	It("keeps egress after an identical NAT gateway PUT", func() {
		tc := setupEgressCase()
		if !tc.dataplane {
			Skip(driftDataplaneSkipMessage)
		}

		By("PUTing the fetched NAT gateway body unchanged except for etag")
		response, responseBody, err := putNatGateway(tc.nat.ID, tc.natBody)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.Properties.ProvisioningState).To(Equal("Succeeded"))
		Expect(canonicalARMBody(responseBody)).To(Equal(canonicalARMBody(tc.natBody)), "identical PUT response should match the original NAT gateway body")
		_, afterBody, err := getNatGateway(tc.nat.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(canonicalARMBody(afterBody)).To(Equal(canonicalARMBody(tc.natBody)), "identical PUT should leave the NAT gateway body unchanged")

		By("Verifying egress remains stable")
		expectEgressHolds(natGatewayDriftStableWindow, func() error {
			return podEgressesFromPIPsErr(ns.Name, tc.podName, tc.pips)
		})
	})

	It("rejects removing the ServiceGateway reference from a NAT gateway", func() {
		tc := setupEgressCase()
		Expect(tc.nat.Properties.ServiceGateway).NotTo(BeNil())
		originalServiceGatewayID := tc.nat.Properties.ServiceGateway.ID

		By("Attempting to remove properties.serviceGateway out of band")
		body := cloneJSONMap(tc.natBody)
		props := jsonObject(body, "properties")
		delete(props, "serviceGateway")
		body["properties"] = props
		out, err := putNatGatewayBody(tc.nat.ID, body)
		Expect(err).To(HaveOccurred())
		Expect(string(out) + err.Error()).To(ContainSubstring("CannotRemoveNatGatewayReferencedInService"))

		By("Verifying the NAT gateway remains linked (and egress is unaffected when observable)")
		after, _, err := getNatGateway(tc.nat.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Properties.ServiceGateway).NotTo(BeNil())
		Expect(after.Properties.ServiceGateway.ID).To(Equal(originalServiceGatewayID))
		if tc.dataplane {
			expectEgressHolds(natGatewayDriftStableWindow, func() error {
				return podEgressesFromPIPsErr(ns.Name, tc.podName, tc.pips)
			})
		}
	})

	It("keeps egress when the NAT gateway idle timeout is changed out of band", Label(nrpKnownIssueLabel), func() {
		if os.Getenv(runKnownIssuesEnv) != "true" {
			Skip("known NRP issue: in-place NAT gateway changes break ServiceGateway egress (reported); set SGW_E2E_RUN_KNOWN_ISSUES=true to run")
		}
		tc := setupEgressCase()
		if !tc.dataplane {
			Skip(driftDataplaneSkipMessage)
		}
		original := cloneJSONMap(tc.natBody)
		registerNATRestore(tc.nat.ID, original, nil)

		By("Changing idleTimeoutInMinutes out of band")
		nextIdleTimeout := 10
		if tc.nat.Properties.IdleTimeoutInMinutes != nil && *tc.nat.Properties.IdleTimeoutInMinutes == nextIdleTimeout {
			nextIdleTimeout = 4
		}
		body := cloneJSONMap(tc.natBody)
		props := jsonObject(body, "properties")
		props["idleTimeoutInMinutes"] = nextIdleTimeout
		body["properties"] = props
		updated, _, err := putNatGateway(tc.nat.ID, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Properties.ProvisioningState).To(Equal("Succeeded"))
		Expect(updated.Properties.IdleTimeoutInMinutes).To(HaveValue(Equal(nextIdleTimeout)))

		By("Verifying egress keeps using one of the NAT gateway Public IPs")
		expectEgressHolds(natGatewayDriftKnownBugWindow, func() error {
			pips, err := getNatGatewayPublicIPs(tc.nat.ID)
			if err != nil {
				return err
			}
			return podEgressesFromPIPsErr(ns.Name, tc.podName, sortedStrings(pips))
		})
	})

	It("keeps egress when a Public IP is added to the NAT gateway out of band", Label(nrpKnownIssueLabel), func() {
		if os.Getenv(runKnownIssuesEnv) != "true" {
			Skip("known NRP issue: in-place NAT gateway changes break ServiceGateway egress (reported); set SGW_E2E_RUN_KNOWN_ISSUES=true to run")
		}
		tc := setupEgressCase()
		if !tc.dataplane {
			Skip(driftDataplaneSkipMessage)
		}
		original := cloneJSONMap(tc.natBody)
		registerNATRestore(tc.nat.ID, original, nil)

		By("Creating an extra StandardV2 IPv4 Public IP")
		extraPIPName := tc.egressName + "-extra-pip"
		extraPublicIPNames = append(extraPublicIPNames, extraPIPName)
		extraPIP, err := createStandardV2IPv4PublicIP(extraPIPName, tc.nat.Location)
		Expect(err).NotTo(HaveOccurred())
		Expect(extraPIP.Properties.IPAddress).NotTo(BeEmpty())

		By("Adding the extra Public IP to the NAT gateway out of band")
		body := cloneJSONMap(tc.natBody)
		props := jsonObject(body, "properties")
		publicIPs, ok := props["publicIpAddresses"].([]any)
		Expect(ok).To(BeTrue(), "NAT gateway properties.publicIpAddresses should be an array")
		props["publicIpAddresses"] = append(publicIPs, map[string]any{"id": extraPIP.ID})
		body["properties"] = props
		updated, _, err := putNatGateway(tc.nat.ID, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Properties.ProvisioningState).To(Equal("Succeeded"))
		Expect(natPublicIPIDs(updated)).To(ContainElement(strings.ToLower(extraPIP.ID)))

		By("Verifying egress keeps using one of the NAT gateway Public IPs")
		expectEgressHolds(natGatewayDriftKnownBugWindow, func() error {
			pips, err := getNatGatewayPublicIPs(tc.nat.ID)
			if err != nil {
				return err
			}
			return podEgressesFromPIPsErr(ns.Name, tc.podName, sortedStrings(pips))
		})
	})

	It("recreates and relinks a NAT gateway deleted out of band", Label(ccmKnownGapLabel), func() {
		if os.Getenv(runKnownIssuesEnv) != "true" {
			Skip("known CCM gap: out-of-band NAT gateway deletion is not repaired; set SGW_E2E_RUN_KNOWN_ISSUES=true to run")
		}
		tc := setupEgressCase()
		originalGUID := tc.nat.Properties.ResourceGUID
		Expect(originalGUID).NotTo(BeEmpty(), "resourceGuid is required to prove the NAT gateway was recreated")

		By("Deleting the identity NAT gateway out of band")
		out, err := runAz("rest", "--method", "delete", "--url", armResourceURL(tc.nat.ID))
		Expect(err).NotTo(HaveOccurred(), string(out))

		By("Verifying the CCM recreates the NAT gateway and relinks the outbound service (and egress when observable)")
		Eventually(func() error {
			service, err := outboundServiceFor(tc.egressName)
			if err != nil {
				return err
			}
			if service.Properties.PublicNatGatewayID == "" {
				return fmt.Errorf("outbound service %s has no publicNatGatewayId", tc.egressName)
			}
			if !strings.HasSuffix(strings.ToLower(service.Properties.PublicNatGatewayID), "/"+strings.ToLower(tc.egressName)) {
				return fmt.Errorf("outbound service points at %s, want NAT gateway named %s", service.Properties.PublicNatGatewayID, tc.egressName)
			}
			nat, _, err := getNatGateway(service.Properties.PublicNatGatewayID)
			if err != nil {
				return err
			}
			if nat.Properties.ResourceGUID == originalGUID {
				return fmt.Errorf("NAT gateway %s still has original resourceGuid %s; waiting for delete/recreate", nat.ID, originalGUID)
			}
			if nat.Properties.ServiceGateway == nil || nat.Properties.ServiceGateway.ID == "" {
				return fmt.Errorf("recreated NAT gateway %s is not linked to the ServiceGateway", nat.ID)
			}
			pips, err := getNatGatewayPublicIPs(nat.ID)
			if err != nil {
				return err
			}
			if len(pips) == 0 {
				return fmt.Errorf("recreated NAT gateway %s has no resolved IPv4 Public IP", nat.ID)
			}
			if len(tc.nat.Properties.PublicIPAddressesV6) > 0 && len(nat.Properties.PublicIPAddressesV6) == 0 {
				return fmt.Errorf("recreated NAT gateway %s has no IPv6 Public IP", nat.ID)
			}
			return tc.egressErr(ns.Name, sortedStrings(pips))
		}, natGatewayDriftRepairWait, natGatewayDriftPollInterval).Should(Succeed())
	})

	It("keeps pod egress and node readiness when default-natgw idle timeout is changed", Label(destructiveLabel), func() {
		if os.Getenv(runDestructiveDefaultNATEnv) != "true" {
			Skip("destructive: changing default-natgw in place can make the whole cluster lose outbound; set SGW_E2E_RUN_DESTRUCTIVE_DEFAULT_NAT=true to run")
		}
		createNamespace()

		By("Creating an unlabelled pod that uses the default NAT gateway")
		const podName = "default-natgw-drift-pod"
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), driftUnlabelledPod(ns.Name, podName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		service, err := defaultOutboundService()
		Expect(err).NotTo(HaveOccurred())
		nat, natBody, err := getNatGateway(service.Properties.PublicNatGatewayID)
		Expect(err).NotTo(HaveOccurred())
		pips, err := getNatGatewayPublicIPs(nat.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(pips).NotTo(BeEmpty(), "default NAT gateway should have an IPv4 Public IP for SNAT observation")

		By("Verifying baseline pod egress through default-natgw")
		assertPodEgressesFromPIPsOrSkip(ns.Name, podName, pips)

		original := cloneJSONMap(natBody)
		registerNATRestore(nat.ID, original, func() error {
			if err := allNodesReadyErr(cs); err != nil {
				return fmt.Errorf("cluster left without egress after restoring default-natgw: %w", err)
			}
			if err := podEgressesFromPIPsErr(ns.Name, podName, sortedStrings(pips)); err != nil {
				return fmt.Errorf("cluster left without egress after restoring default-natgw: %w", err)
			}
			return nil
		})

		By("Changing default-natgw idleTimeoutInMinutes out of band")
		nextIdleTimeout := 10
		if nat.Properties.IdleTimeoutInMinutes != nil && *nat.Properties.IdleTimeoutInMinutes == nextIdleTimeout {
			nextIdleTimeout = 4
		}
		body := cloneJSONMap(natBody)
		props := jsonObject(body, "properties")
		props["idleTimeoutInMinutes"] = nextIdleTimeout
		body["properties"] = props
		updated, _, err := putNatGateway(nat.ID, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Properties.ProvisioningState).To(Equal("Succeeded"))
		Expect(updated.Properties.IdleTimeoutInMinutes).To(HaveValue(Equal(nextIdleTimeout)))

		By("Verifying pod egress and node readiness stay healthy")
		expectEgressHolds(natGatewayDriftKnownBugWindow, func() error {
			if err := allNodesReadyErr(cs); err != nil {
				return err
			}
			return podEgressesFromPIPsErr(ns.Name, podName, sortedStrings(pips))
		})
	})
})

func randomNatGatewayDriftName() string {
	return "sgwdrift-" + strings.ToLower(string(uuid.NewUUID())[:8])
}

func driftEgressPod(namespace, name, egressName string) *v1.Pod {
	pod := driftUnlabelledPod(namespace, name)
	pod.Labels = map[string]string{egressLabel: egressName}
	return pod
}

func driftUnlabelledPod(namespace, name string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Name:            "test-app",
				Image:           utils.AgnhostImage,
				ImagePullPolicy: v1.PullIfNotPresent,
				Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", natGatewayDriftTargetPort)},
			}},
		},
	}
}

func outboundServiceFor(egressName string) (ServiceGatewayService, error) {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return ServiceGatewayService{}, err
	}
	for _, svc := range sgResponse.Value {
		if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
			if svc.Properties.PublicNatGatewayID == "" {
				return ServiceGatewayService{}, fmt.Errorf("outbound service %s has no publicNatGatewayId", egressName)
			}
			return svc, nil
		}
	}
	return ServiceGatewayService{}, fmt.Errorf("outbound service %s not found", egressName)
}

func defaultOutboundService() (ServiceGatewayService, error) {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return ServiceGatewayService{}, err
	}
	for _, svc := range sgResponse.Value {
		if svc.Properties.ServiceType == "Outbound" && (svc.Name == defaultOutboundServiceName || svc.Properties.IsDefault) {
			if svc.Properties.PublicNatGatewayID == "" {
				return ServiceGatewayService{}, fmt.Errorf("default outbound service %s has no publicNatGatewayId", svc.Name)
			}
			return svc, nil
		}
	}
	return ServiceGatewayService{}, fmt.Errorf("default outbound service %s not found", defaultOutboundServiceName)
}

func getNatGateway(natGatewayID string) (natGatewayDriftResource, map[string]any, error) {
	output, err := runAz("rest", "--method", "get", "--url", armResourceURL(natGatewayID))
	if err != nil {
		return natGatewayDriftResource{}, nil, fmt.Errorf("get NAT gateway %s: %w, output: %s", natGatewayID, err, string(output))
	}
	return decodeNatGateway(output)
}

func putNatGateway(natGatewayID string, body map[string]any) (natGatewayDriftResource, map[string]any, error) {
	output, err := putNatGatewayBody(natGatewayID, body)
	if err != nil {
		return natGatewayDriftResource{}, nil, err
	}
	nat, natBody, err := decodeNatGateway(output)
	if err != nil {
		return nat, natBody, err
	}
	deadline := time.Now().Add(natGatewayDriftProvisionWait)
	for nat.Properties.ProvisioningState != "Succeeded" && nat.Properties.ProvisioningState != "Failed" && time.Now().Before(deadline) {
		time.Sleep(natGatewayDriftPollInterval)
		if nat, natBody, err = getNatGateway(natGatewayID); err != nil {
			return nat, natBody, err
		}
	}
	if nat.Properties.ProvisioningState != "Succeeded" {
		return nat, natBody, fmt.Errorf("NAT gateway %s ended in provisioningState %q", natGatewayID, nat.Properties.ProvisioningState)
	}
	return nat, natBody, nil
}

func putNatGatewayBody(natGatewayID string, body map[string]any) ([]byte, error) {
	body = cloneJSONMap(body)
	delete(body, "etag")
	payload, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	return runAz("rest", "--method", "put", "--url", armResourceURL(natGatewayID), "--body", string(payload))
}

func decodeNatGateway(output []byte) (natGatewayDriftResource, map[string]any, error) {
	var nat natGatewayDriftResource
	if err := json.Unmarshal(output, &nat); err != nil {
		return natGatewayDriftResource{}, nil, fmt.Errorf("parse NAT gateway JSON: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(output, &body); err != nil {
		return natGatewayDriftResource{}, nil, fmt.Errorf("parse NAT gateway JSON map: %w", err)
	}
	return nat, body, nil
}

func createStandardV2IPv4PublicIP(name, location string) (publicIPDriftResource, error) {
	id := publicIPResourceID(name)
	body := map[string]any{
		"location": location,
		"sku": map[string]any{
			"name": "StandardV2",
		},
		"properties": map[string]any{
			"publicIPAddressVersion":   "IPv4",
			"publicIPAllocationMethod": "Static",
		},
		"tags": map[string]any{natGatewayDriftTestTag: name},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return publicIPDriftResource{}, err
	}
	output, err := runAz("rest", "--method", "put", "--url", armResourceURL(id), "--body", string(payload))
	if err != nil {
		return publicIPDriftResource{}, fmt.Errorf("create Public IP %s: %w, output: %s", name, err, string(output))
	}

	var pip publicIPDriftResource
	if err := json.Unmarshal(output, &pip); err != nil {
		return publicIPDriftResource{}, fmt.Errorf("parse Public IP %s: %w", name, err)
	}
	if pip.ID == "" {
		pip.ID = id
	}
	Eventually(func() (string, error) {
		current, err := getPublicIP(name)
		if err != nil {
			return "", err
		}
		return current.Properties.IPAddress, nil
	}, natGatewayDriftProvisionWait, natGatewayDriftPollInterval).ShouldNot(BeEmpty())
	current, err := getPublicIP(name)
	if err != nil {
		return publicIPDriftResource{}, err
	}
	return current, nil
}

func getPublicIP(name string) (publicIPDriftResource, error) {
	output, err := runAz("rest", "--method", "get", "--url", armResourceURL(publicIPResourceID(name)))
	if err != nil {
		return publicIPDriftResource{}, fmt.Errorf("get Public IP %s: %w, output: %s", name, err, string(output))
	}
	var pip publicIPDriftResource
	if err := json.Unmarshal(output, &pip); err != nil {
		return publicIPDriftResource{}, fmt.Errorf("parse Public IP %s: %w", name, err)
	}
	return pip, nil
}

func deletePublicIPNamedErr(name string) error {
	output, err := runAz("rest", "--method", "delete", "--url", armResourceURL(publicIPResourceID(name)))
	if err != nil && !isNotFoundAzureOutput(output, err) {
		return fmt.Errorf("delete Public IP %s: %w, output: %s", name, err, string(output))
	}
	return azurePublicIPNamedAbsentErr(name)
}

func podEgressesFromPIPsErr(namespace, podName string, pips []string) error {
	if len(pips) == 0 {
		return fmt.Errorf("no NAT gateway Public IPs supplied")
	}
	observed, err := probePodIPv4Egress(namespace, podName)
	if err != nil {
		return fmt.Errorf("pod %s/%s: %w", namespace, podName, err)
	}
	for _, pip := range pips {
		if observed == pip {
			return nil
		}
	}
	return fmt.Errorf("pod %s/%s egressed as %s, which is not one of the NAT gateway Public IPs %v", namespace, podName, observed, pips)
}

func probePodIPv4Egress(namespace, podName string) (string, error) {
	stdout, _, err := utils.NewKubectlCommand(namespace, "exec", podName, "--", "/bin/sh", "-c", "curl -4 -s -m 10 ifconfig.me/ip").ExecWithFullOutput(false)
	ip := net.ParseIP(strings.TrimSpace(stdout))
	if err != nil || ip == nil || ip.To4() == nil {
		return "", errNoEgressSource
	}
	return ip.String(), nil
}

// expectEgressHolds keeps check passing for window, tolerating up to natGatewayDriftMaxEmptyProbes-1
// consecutive unanswered probes, and then requires one more answered probe so a loss at the end is caught.
func expectEgressHolds(window time.Duration, check func() error) {
	misses := 0
	Consistently(func() error {
		err := check()
		if err == nil {
			misses = 0
			return nil
		}
		if !errors.Is(err, errNoEgressSource) {
			return err
		}
		misses++
		if misses >= natGatewayDriftMaxEmptyProbes {
			return err
		}
		utils.Logf("Ignoring probe %d without an outbound source: %v", misses, err)
		return nil
	}, window, natGatewayDriftPollInterval).Should(Succeed())
	Eventually(check, 2*natGatewayDriftMaxEmptyProbes*natGatewayDriftPollInterval, natGatewayDriftPollInterval).Should(Succeed())
}

func pollUntilSucceeds(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(natGatewayDriftPollInterval)
	}
}

func assertPodEgressesFromPIPsOrSkip(namespace, podName string, pips []string) {
	if !podEgressObservable(namespace, podName, pips) {
		Skip(driftDataplaneSkipMessage)
	}
}

// podEgressObservable reports whether the pod's egress can be observed; an observed source that is not one
// of pips fails the spec.
func podEgressObservable(namespace, podName string, pips []string) bool {
	var observed string
	for i := 0; i < natGatewayDriftMaxEmptyProbes && observed == ""; i++ {
		if i > 0 {
			time.Sleep(natGatewayDriftPollInterval)
		}
		observed, _ = probePodIPv4Egress(namespace, podName)
	}
	if observed == "" {
		return false
	}
	Expect(sortedStrings(pips)).To(ContainElement(observed),
		"pod %s/%s egressed as %s, which is not one of the NAT gateway Public IPs %v", namespace, podName, observed, pips)
	return true
}

func allNodesReadyErr(cs clientset.Interface) error {
	nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no nodes found")
	}
	for i := range nodes.Items {
		ready := false
		for _, condition := range nodes.Items[i].Status.Conditions {
			if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("node %s is not Ready", nodes.Items[i].Name)
		}
	}
	return nil
}

func armResourceURL(resourceID string) string {
	ensureSLBConfigInitialized()
	return fmt.Sprintf("https://management.azure.com%s?api-version=%s", resourceID, apiVersion)
}

func publicIPResourceID(name string) string {
	ensureSLBConfigInitialized()
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", subscriptionID, resourceGroupName, name)
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, err := json.Marshal(in)
	Expect(err).NotTo(HaveOccurred())
	var out map[string]any
	Expect(json.Unmarshal(data, &out)).To(Succeed())
	return out
}

func canonicalARMBody(in map[string]any) map[string]any {
	out := cloneJSONMap(in)
	delete(out, "etag")
	return out
}

func jsonObject(in map[string]any, key string) map[string]any {
	if raw, ok := in[key].(map[string]any); ok {
		return raw
	}
	return map[string]any{}
}

func natPublicIPIDs(nat natGatewayDriftResource) []string {
	var ids []string
	for _, ref := range append(append([]armIDReference(nil), nat.Properties.PublicIPAddresses...), nat.Properties.PublicIPAddressesV6...) {
		ids = append(ids, strings.ToLower(ref.ID))
	}
	return sortedStrings(ids)
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func isNotFoundAzureOutput(output []byte, err error) bool {
	text := string(output)
	if err != nil {
		text += err.Error()
	}
	return strings.Contains(text, "NotFound") || strings.Contains(text, "not found") || strings.Contains(text, "ResourceNotFound")
}
