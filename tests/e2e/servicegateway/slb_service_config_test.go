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
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

type nicNSGRule struct {
	Name                       string   `json:"name"`
	Description                string   `json:"description"`
	Access                     string   `json:"access"`
	Direction                  string   `json:"direction"`
	Protocol                   string   `json:"protocol"`
	Priority                   int32    `json:"priority"`
	SourceAddressPrefix        string   `json:"sourceAddressPrefix"`
	SourceAddressPrefixes      []string `json:"sourceAddressPrefixes"`
	DestinationAddressPrefix   string   `json:"destinationAddressPrefix"`
	DestinationAddressPrefixes []string `json:"destinationAddressPrefixes"`
	SourcePortRange            string   `json:"sourcePortRange"`
	SourcePortRanges           []string `json:"sourcePortRanges"`
	DestinationPortRange       string   `json:"destinationPortRange"`
	DestinationPortRanges      []string `json:"destinationPortRanges"`
}

type nicNSG struct {
	ID    string            `json:"id"`
	Tags  map[string]string `json:"tags"`
	Rules []nicNSGRule      `json:"rules"`
	State string            `json:"state"`
	NICs  []struct {
		ID string `json:"id"`
	} `json:"nics"`
}

func readNICNSGs() ([]nicNSG, error) {
	query := fmt.Sprintf("[?starts_with(name,'%s')].{id:id,tags:tags,rules:securityRules,state:provisioningState,nics:networkInterfaces}", consts.ServiceGatewayNSGNamePrefix)
	output, err := runAz("network", "nsg", "list", "--subscription", subscriptionID,
		"--resource-group", resourceGroupName, "--query", query, "--output", "json")
	if err != nil {
		return nil, err
	}
	var groups []nicNSG
	if err := json.Unmarshal(output, &groups); err != nil {
		return nil, fmt.Errorf("decode managed NIC NSGs: %w", err)
	}
	return groups, nil
}

func nicNSGValues(single string, multiple []string) []string {
	values := slices.Clone(multiple)
	if single != "" {
		values = append(values, single)
	}
	for i, value := range values {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			values[i] = prefix.Masked().String()
		} else if address, err := netip.ParseAddr(value); err == nil {
			values[i] = address.Unmap().String()
		}
	}
	slices.Sort(values)
	return slices.Compact(values)
}

func nicNSGServiceRules(group nicNSG, uid string) []nicNSGRule {
	var rules []nicNSGRule
	for _, rule := range group.Rules {
		if rule.Description == consts.ServiceGatewayNSGServiceDescriptionPrefix+inboundServiceName(uid) {
			rules = append(rules, rule)
		}
	}
	return rules
}

func nicNSGPolicyErr(group nicNSG, uid, source string, addresses []string, port int32) error {
	rules := nicNSGServiceRules(group, uid)
	if len(rules) != 2 {
		return fmt.Errorf("NSG %s has %d service rules, want one allow and one deny", group.ID, len(rules))
	}
	var allow, deny int32
	for _, rule := range rules {
		if rule.Direction != "Inbound" || rule.Protocol != "Tcp" ||
			!slices.Equal(nicNSGValues(rule.SourcePortRange, rule.SourcePortRanges), []string{"*"}) ||
			!slices.Equal(nicNSGValues(rule.DestinationPortRange, rule.DestinationPortRanges), []string{strconv.Itoa(int(port))}) ||
			!slices.Equal(nicNSGValues(rule.DestinationAddressPrefix, rule.DestinationAddressPrefixes), nicNSGValues("", addresses)) {
			return fmt.Errorf("NSG %s rule %s does not match the backend tuples", group.ID, rule.Name)
		}
		sources := nicNSGValues(rule.SourceAddressPrefix, rule.SourceAddressPrefixes)
		switch {
		case rule.Access == "Allow" && slices.Equal(sources, nicNSGValues("", strings.Split(source, ","))):
			allow = rule.Priority
		case rule.Access == "Deny" && slices.Equal(sources, []string{"*"}):
			deny = rule.Priority
		default:
			return fmt.Errorf("NSG %s rule %s has unexpected source policy", group.ID, rule.Name)
		}
	}
	if allow == 0 || deny <= allow {
		return fmt.Errorf("NSG %s does not allow selected sources before denying other sources", group.ID)
	}
	return nil
}

func checkNICNSGPolicies(uid, source string, addresses map[string][]string, port int32) (map[string]string, error) {
	groups, err := readNICNSGs()
	if err != nil {
		return nil, err
	}
	found := make(map[string]string)
	for _, group := range groups {
		if len(nicNSGServiceRules(group, uid)) == 0 {
			continue
		}
		node := group.Tags[consts.ServiceGatewayNSGNodeTag]
		if _, wanted := addresses[node]; !wanted || found[node] != "" {
			return nil, fmt.Errorf("unexpected or duplicate NSG for node %s", node)
		}
		if group.State != "Succeeded" || len(group.NICs) != 1 ||
			group.Tags[consts.ServiceGatewayNSGOwnerTag] == "" || group.Tags[consts.ServiceGatewayNSGNICTag] == "" {
			return nil, fmt.Errorf("NSG %s has incomplete ownership, provisioning or NIC association", group.ID)
		}
		if err := nicNSGPolicyErr(group, uid, source, addresses[node], port); err != nil {
			return nil, err
		}
		output, err := runAz("rest", "--method", "get", "--url",
			fmt.Sprintf("https://management.azure.com%s?api-version=2018-10-01", group.NICs[0].ID),
			"--query", "properties.networkSecurityGroup.id", "--output", "json")
		if err != nil {
			return nil, err
		}
		var associated string
		if err := json.Unmarshal(output, &associated); err != nil {
			return nil, fmt.Errorf("decode NIC NSG association: %w", err)
		}
		if !strings.EqualFold(associated, group.ID) {
			return nil, fmt.Errorf("NIC %s is associated with %s, want %s", group.NICs[0].ID, associated, group.ID)
		}
		found[node] = group.ID
	}
	if len(found) != len(addresses) {
		return nil, fmt.Errorf("found %d protected NICs, want %d", len(found), len(addresses))
	}
	return found, nil
}

// Edge cases around the inbound service shape: a service with many distinct ports, and several
// services that select the same pods, including NIC source policies. These assert the
// cloud-provider contract and are independent of the environment dataplane.
var _ = Describe("SLB - Service Config Edge Cases", Label(slbTestLabel), func() {
	basename := "slb-service-config-test"

	var (
		cs               clientset.Interface
		ns               *v1.Namespace
		sourcePolicyUIDs []string
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
		sourcePolicyUIDs = nil
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			Expect(utils.DeleteNamespace(cs, ns.Name)).To(Succeed())

			if len(sourcePolicyUIDs) > 0 {
				Eventually(func() error {
					groups, err := readNICNSGs()
					if err != nil {
						return err
					}
					for _, group := range groups {
						for _, uid := range sourcePolicyUIDs {
							if len(nicNSGServiceRules(group, uid)) != 0 {
								return fmt.Errorf("NSG %s still protects deleted service %s", group.ID, uid)
							}
						}
					}
					return nil
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
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
		eventuallyServiceReconciled(cs, ns.Name, serviceUID, numPods, waitTime)

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
		eventuallyServiceReconciled(cs, ns.Name, uidA, numPods, waitTime)
		eventuallyServiceReconciled(cs, ns.Name, uidB, numPods, waitTime)

		utils.Logf("\n✓ Shared-pod services each registered %d pods (UIDs %s, %s)", numPods, uidA, uidB)
	})

	Context("managed NIC source policies", Label("SLB-NIC-NSG"), func() {
		newService := func(name, source string, family v1.IPFamily, targetPort int32) *v1.Service {
			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "nsg-backend"},
					IPFamilies: []v1.IPFamily{family},
					Ports:      []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(targetPort), Protocol: v1.ProtocolTCP}},
				},
			}
			if source != "" {
				svc.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: source}
			}
			created, err := cs.CoreV1().Services(ns.Name).Create(context.Background(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			sourcePolicyUIDs = append(sourcePolicyUIDs, string(created.UID))
			return created
		}

		updateService := func(name string, edit func(*v1.Service)) {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				service, err := cs.CoreV1().Services(ns.Name).Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				edit(service)
				_, err = cs.CoreV1().Services(ns.Name).Update(context.Background(), service, metav1.UpdateOptions{})
				return err
			})).To(Succeed())
		}

		DescribeTable("should update and remove source policies on each backend NIC",
			func(family v1.IPFamily, initial, changed string) {
				ctx := context.Background()
				nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				var selected []string
				for _, node := range nodes.Items {
					ready := false
					for _, condition := range node.Status.Conditions {
						ready = ready || condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue
					}
					for _, address := range node.Status.Addresses {
						ip, err := netip.ParseAddr(address.Address)
						if ready && !node.Spec.Unschedulable && address.Type == v1.NodeInternalIP && err == nil &&
							ip.Is6() == (family == v1.IPv6Protocol) {
							selected = append(selected, node.Name)
							break
						}
					}
				}
				if len(selected) < 2 {
					Skip(fmt.Sprintf("requires two Ready nodes with %s addresses", family))
				}
				selected = selected[:2]
				for i, node := range selected {
					pod := makeNetexecPod(fmt.Sprintf("nsg-pod-%d", i), map[string]string{"app": "nsg-backend"}, 8080)
					pod.Spec.NodeName = node
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
				pods, err := cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: "app=nsg-backend"})
				Expect(err).NotTo(HaveOccurred())
				addresses := make(map[string][]string)
				registered := make(map[string]struct{})
				for _, pod := range pods.Items {
					for _, address := range pod.Status.PodIPs {
						ip, err := netip.ParseAddr(address.IP)
						Expect(err).NotTo(HaveOccurred())
						if ip.Is6() == (family == v1.IPv6Protocol) {
							addresses[pod.Spec.NodeName] = append(addresses[pod.Spec.NodeName], ip.String())
							registered[ip.String()] = struct{}{}
						}
					}
				}
				if len(addresses) != len(selected) {
					Skip(fmt.Sprintf("requires %s Pod addresses on both nodes", family))
				}
				service := newService("source-policy", initial, family, 8080)
				uid := string(service.UID)
				By("Verifying source rules and the NIC-side associations before accepting membership")
				var ids map[string]string
				Eventually(func() error {
					found, err := checkNICNSGPolicies(uid, initial, addresses, 8080)
					if err == nil {
						ids = found
					}
					return err
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
				Eventually(func() error { return registeredAddressesMatchErr(inboundServiceName(uid), registered) },
					5*time.Minute, defaultPollInterval).Should(Succeed())

				By("Changing only the annotation, without endpoint changes")
				updateService(service.Name, func(svc *v1.Service) {
					svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = changed
				})
				Eventually(func() error {
					_, err := checkNICNSGPolicies(uid, changed, addresses, 8080)
					return err
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
				Expect(registeredAddressesMatchErr(inboundServiceName(uid), registered)).To(Succeed())

				By("Changing the backend port and retiring the old port's source rules")
				updateService(service.Name, func(svc *v1.Service) {
					svc.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
				})
				Eventually(func() error {
					rules, err := getLoadBalancerRules(uid)
					if err != nil {
						return err
					}
					if len(rules) != 1 || rules[0].BackendPort != 9090 {
						return fmt.Errorf("LB has not applied the new backend port")
					}
					_, err = checkNICNSGPolicies(uid, changed, addresses, 9090)
					return err
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())

				By("Removing the restriction while keeping the NIC NSGs and backend memberships")
				updateService(service.Name, func(svc *v1.Service) {
					delete(svc.Annotations, consts.ServiceAnnotationAllowedIPRanges)
				})
				Eventually(func() error {
					groups, err := readNICNSGs()
					if err != nil {
						return err
					}
					for node, id := range ids {
						found := false
						for _, group := range groups {
							if strings.EqualFold(group.ID, id) {
								found = true
								if len(nicNSGServiceRules(group, uid)) != 0 || len(group.NICs) != 1 {
									return fmt.Errorf("NSG for node %s has not removed only the restriction", node)
								}
							}
						}
						if !found {
							return fmt.Errorf("NSG for live node %s was deleted", node)
						}
					}
					return registeredAddressesMatchErr(inboundServiceName(uid), registered)
				}, 5*time.Minute, defaultPollInterval).Should(Succeed())
			},
			Entry("IPv4", v1.IPv4Protocol, "20.0.0.0/8", "40.0.0.0/8"),
			Entry("IPv6", v1.IPv6Protocol, "2001:db8::/32", "2001:db9::/32"),
		)

		It("should reject overlapping backend policies and recover when their ports no longer overlap", func() {
			ctx := context.Background()
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx,
				makeNetexecPod("shared-nsg-pod", map[string]string{"app": "nsg-backend"}, 8080), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())
			unrestricted := newService("unrestricted", "", v1.IPv4Protocol, 8080)
			eventuallyServiceReconciled(cs, ns.Name, string(unrestricted.UID), 1, 5*time.Minute)
			restricted := newService("restricted", "20.0.0.0/8", v1.IPv4Protocol, 8080)
			Eventually(func() (bool, error) {
				events, err := cs.CoreV1().Events(ns.Name).List(ctx, metav1.ListOptions{
					FieldSelector: fmt.Sprintf("involvedObject.uid=%s,reason=%s", restricted.UID, consts.ServiceGatewaySourceRangeConflictReason),
				})
				if err != nil {
					return false, err
				}
				return len(events.Items) > 0, nil
			}, 5*time.Minute, defaultPollInterval).Should(BeTrue())
			Consistently(func() (int, error) {
				return countRegisteredEndpoints(inboundServiceName(string(restricted.UID)))
			}, 20*time.Second, defaultPollInterval).Should(Equal(0))
			eventuallyServiceReconciled(cs, ns.Name, string(unrestricted.UID), 1, time.Minute)

			By("Correcting the overlap without a CCM restart")
			updateService(restricted.Name, func(svc *v1.Service) {
				svc.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			})
			eventuallyServiceReconciled(cs, ns.Name, string(restricted.UID), 1, 5*time.Minute)
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, "shared-nsg-pod", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			want := make(map[string][]string)
			for _, address := range pod.Status.PodIPs {
				ip, err := netip.ParseAddr(address.IP)
				Expect(err).NotTo(HaveOccurred())
				if ip.Is4() {
					want[pod.Spec.NodeName] = append(want[pod.Spec.NodeName], ip.String())
				}
			}
			Expect(want).To(HaveLen(1))
			Eventually(func() error {
				_, err := checkNICNSGPolicies(string(restricted.UID), "20.0.0.0/8", want, 9090)
				return err
			}, 5*time.Minute, defaultPollInterval).Should(Succeed())
			Expect(cs.CoreV1().Services(ns.Name).Delete(ctx, restricted.Name, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() error { return serviceDeletedErr(string(restricted.UID)) },
				5*time.Minute, defaultPollInterval).Should(Succeed())
			eventuallyServiceReconciled(cs, ns.Name, string(unrestricted.UID), 1, time.Minute)
		})
	})
})

func TestNICNSGPolicyProjection(t *testing.T) {
	allow := nicNSGRule{
		Name: "allow", Description: consts.ServiceGatewayNSGServiceDescriptionPrefix + "svc-lb",
		Direction: "Inbound", Access: "Allow", Protocol: "Tcp", Priority: 100,
		SourceAddressPrefix: "2001:db8::/32", SourcePortRange: "*",
		DestinationAddressPrefixes: []string{"fd00::1", "fd00::2"}, DestinationPortRanges: []string{"8080"},
	}
	deny := allow
	deny.Name, deny.Access, deny.Priority, deny.SourceAddressPrefix = "deny", "Deny", 200, "*"
	for _, tt := range []struct {
		name   string
		change func(*nicNSG)
		valid  bool
	}{
		{name: "both rules", change: func(*nicNSG) {}, valid: true},
		{name: "IPv6 spelling and order", valid: true, change: func(group *nicNSG) {
			for i := range group.Rules {
				group.Rules[i].DestinationAddressPrefixes = []string{"FD00::2", "fd00::1"}
			}
		}},
		{name: "missing deny", change: func(group *nicNSG) { group.Rules = group.Rules[:1] }},
		{name: "legacy raw UID description", change: func(group *nicNSG) {
			for i := range group.Rules {
				group.Rules[i].Description = consts.ServiceGatewayNSGServiceDescriptionPrefix + "svc"
			}
		}},
		{name: "wrong priority", change: func(group *nicNSG) { group.Rules[1].Priority = 99 }},
		{name: "old source", change: func(group *nicNSG) { group.Rules[0].SourceAddressPrefix = "2001:db9::/32" }},
		{name: "old port", change: func(group *nicNSG) { group.Rules[1].DestinationPortRanges = []string{"9090"} }},
		{name: "extra address", change: func(group *nicNSG) { group.Rules[1].DestinationAddressPrefix = "fd00::3" }},
		{name: "wrong protocol", change: func(group *nicNSG) { group.Rules[1].Protocol = "Udp" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			group := nicNSG{ID: "nsg", Rules: []nicNSGRule{allow, deny}}
			tt.change(&group)
			err := nicNSGPolicyErr(group, "svc", "2001:db8::/32", []string{"fd00::1", "fd00::2"}, 8080)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t, error=%v", tt.valid, err)
			}
		})
	}
}

func TestNICNSGPolicyProjectionMultipleSources(t *testing.T) {
	allow := nicNSGRule{
		Name: "allow", Description: consts.ServiceGatewayNSGServiceDescriptionPrefix + "svc-lb",
		Direction: "Inbound", Access: "Allow", Protocol: "Tcp", Priority: 500,
		SourceAddressPrefixes: []string{"20.1.2.3/32", "10.1.2.3/32"}, SourcePortRange: "*",
		DestinationAddressPrefix: "10.0.0.1", DestinationPortRange: "8080",
	}
	deny := allow
	deny.Name, deny.Access, deny.Priority = "deny", "Deny", 4095
	deny.SourceAddressPrefixes, deny.SourceAddressPrefix = nil, "*"
	group := nicNSG{ID: "nsg", Rules: []nicNSGRule{allow, deny}}
	g := NewWithT(t)
	g.Expect(nicNSGPolicyErr(group, "svc", "10.1.2.3/32,20.1.2.3/32", []string{"10.0.0.1"}, 8080)).To(Succeed())
	g.Expect(nicNSGPolicyErr(group, "svc", "10.1.2.3/32,30.1.2.3/32", []string{"10.0.0.1"}, 8080)).NotTo(Succeed())
}

func TestNICNSGAssociationReadAPIVersion(t *testing.T) {
	oldSubscription, oldGroup, oldVersion := subscriptionID, resourceGroupName, apiVersion
	subscriptionID, resourceGroupName, apiVersion = "sub", "rg", "2026-01-01"
	t.Cleanup(func() { subscriptionID, resourceGroupName, apiVersion = oldSubscription, oldGroup, oldVersion })
	for _, resource := range []string{
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic",
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/pool/virtualMachines/0/networkInterfaces/nic",
	} {
		t.Run(resource, func(t *testing.T) {
			allow := nicNSGRule{
				Name: "allow", Description: consts.ServiceGatewayNSGServiceDescriptionPrefix + "svc-lb",
				Direction: "Inbound", Access: "Allow", Protocol: "Tcp", Priority: 500,
				SourceAddressPrefix: "20.0.0.0/8", SourcePortRange: "*",
				DestinationAddressPrefix: "10.1.0.1", DestinationPortRange: "8080",
			}
			deny := allow
			deny.Name, deny.Access, deny.Priority, deny.SourceAddressPrefix = "deny", "Deny", 4095, "*"
			group := nicNSG{
				ID:    "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/sgw-nsg-test",
				State: "Succeeded", Rules: []nicNSGRule{allow, deny},
				Tags: map[string]string{
					consts.ServiceGatewayNSGNodeTag: "node", consts.ServiceGatewayNSGOwnerTag: "owner",
					consts.ServiceGatewayNSGNICTag: "nic",
				},
			}
			group.NICs = append(group.NICs, struct {
				ID string `json:"id"`
			}{ID: resource})
			g := NewWithT(t)
			model, err := json.Marshal([]nicNSG{group})
			g.Expect(err).NotTo(HaveOccurred())
			directory := t.TempDir()
			script := fmt.Sprintf(`#!/bin/sh
case "$*" in
    "network nsg list"*) printf '%%s\n' '%s' ;;
    *"https://management.azure.com%s?api-version=2018-10-01"*) printf '%%s\n' '"%s"' ;;
    *) echo "unsupported NIC read API" >&2; exit 9 ;;
esac
`, model, resource, group.ID)
			// #nosec G306 -- owner-only executable Azure CLI fixture.
			g.Expect(os.WriteFile(filepath.Join(directory, "az"), []byte(script), 0o700)).To(Succeed())
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			found, err := checkNICNSGPolicies("svc", "20.0.0.0/8", map[string][]string{"node": {"10.1.0.1"}}, 8080)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(Equal(map[string]string{"node": group.ID}))
		})
	}
}
