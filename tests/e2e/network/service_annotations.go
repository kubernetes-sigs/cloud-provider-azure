/*
Copyright 2018 The Kubernetes Authors.

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
	"net/http"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"
	"k8s.io/legacy-cloud-providers/azure"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var (
	scalesetRE               = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
	lbNameRE                 = regexp.MustCompile(`^/subscriptions/(?:.)/resourceGroups/(?:.)/providers/Microsoft.Network/loadBalancers/(.+)/frontendIPConfigurations(?:.*)`)
	backendIPConfigurationRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
)

const (
	nginxPort       = 80
	nginxStatusCode = 200
	pullInterval    = 20 * time.Second
	pullTimeout     = 10 * time.Minute
)

var _ = Describe("Service with annotation", func() {
	basename := "service"
	serviceName := "annotation-test"

	var cs clientset.Interface
	var ns *v1.Namespace

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		Port:       nginxPort,
		TargetPort: intstr.FromInt(nginxPort),
	}}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createNginxDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(deployment)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(serviceName, nil)
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
	})

	It("can be accessed by domain name", func() {
		By("Create service")
		serviceDomainNamePrefix := serviceName + string(uuid.NewUUID())

		annotation := map[string]string{
			azure.ServiceAnnotationDNSLabelName: serviceDomainNamePrefix,
		}

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for service exposure")
		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Validating External domain name")
		var code int
		serviceDomainName := utils.GetServiceDomainName(serviceDomainNamePrefix)
		url := fmt.Sprintf("http://%s:%v", serviceDomainName, ports[0].Port)
		for i := 1; i <= 30; i++ {
			resp, err := http.Get(url)
			if err == nil {
				defer func() {
					if resp != nil {
						resp.Body.Close()
					}
				}()
				code = resp.StatusCode
				if resp.StatusCode == nginxStatusCode {
					break
				}
			}
			time.Sleep(20 * time.Second)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(nginxStatusCode), "Fail to get response from the domain name")
	})

	It("can be bound to an internal load balancer", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "true",
		}

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for service exposure")
		ip, err := utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Validating whether the load balancer is internal")
		url := fmt.Sprintf("%s:%v", ip, ports[0].Port)
		err = validateInternalLoadBalancer(cs, ns.Name, url)
		Expect(err).NotTo(HaveOccurred())
	})

	It("can specify which subnet the internal load balancer should be bound to", func() {
		By("creating environment")
		subnetName := "lb-subnet"

		azureTestClient, err := utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())
		vNet, err := azureTestClient.GetClusterVirtualNetwork()
		Expect(err).NotTo(HaveOccurred())
		newSubnetCIDR, err := utils.GetNextSubnetCIDR(vNet)
		Expect(err).NotTo(HaveOccurred())

		azureTestClient.CreateSubnet(vNet, &subnetName, &newSubnetCIDR)

		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal:       "true",
			azure.ServiceAnnotationLoadBalancerInternalSubnet: subnetName,
		}

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred(), "Warning: Subnet %s cannot be delete since fail to delete the bounding service %s", subnetName, serviceName)

			err = azureTestClient.DeleteSubnet(*vNet.Name, subnetName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for service exposure")
		ip, err := utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Get External IP: %s", ip)

		By("Validating external ip in target subnet")
		ret, err := utils.ValidateIPInCIDR(ip, newSubnetCIDR)
		Expect(err).NotTo(HaveOccurred())
		Expect(ret).To(Equal(true), "external ip %s is not in the target subnet %s", ip, newSubnetCIDR)
	})

	It("should bound to the specified load balancer from azure-load-balancer-mode [MultipleAgentPools][VMSS]", func() {
		//get nodelist and providerID specific to an agentnodes
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		// Get all the vmss names from all node's providerIDs
		vmssname := sets.NewString()
		var resourceGroupName string
		for _, node := range nodes {
			if utils.IsMasterNode(&node) {
				continue
			}
			providerID := node.Spec.ProviderID
			matches := scalesetRE.FindStringSubmatch(providerID)
			if len(matches) != 3 {
				Skip("Report error")
			}
			vmss := matches[1]
			resourceGroupName = matches[2]
			vmssname.Insert(vmss)
		}
		Expect(resourceGroupName).NotTo(Equal(""))
		//Skip if there're less than two vmss nodes
		if len(vmssname) < 2 {
			Skip("azure-load-balancer-mode tests only works for cluster with multiple vmss agent pools")
		}
		vmssList := vmssname.List()[:2]

		for _, vmss := range vmssList {
			validateLoadBalancerBackendPools(vmss, cs, labels, ns.Name, ports, resourceGroupName)
		}
	})
})

func createLoadBalancerServiceManifest(c clientset.Interface, name string, annotation map[string]string, labels map[string]string, namespace string, ports []v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotation,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Ports:    ports,
			Type:     "LoadBalancer",
		},
	}
}

// defaultDeployment returns a default deployment
// running nginx image which exposes port 80
func createNginxDeploymentManifest(name string, labels map[string]string) (result *appsv1.Deployment) {
	var replicas int32
	replicas = 5
	result = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Hostname: name,
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           "nginx:1.15",
							ImagePullPolicy: "Always",
							Ports: []v1.ContainerPort{
								{
									ContainerPort: nginxPort,
								},
							},
						},
					},
				},
			},
		},
	}
	return
}

// validate internal source can access to ILB
func validateInternalLoadBalancer(c clientset.Interface, ns string, url string) error {
	// create a pod to access to the service
	utils.Logf("Validating external IP not be public and internal accessible")
	utils.Logf("Create a front pod to connect to service")
	podName := "front-pod"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Hostname: podName,
			Containers: []v1.Container{
				{
					Name:            "test-app",
					Image:           "appropriate/curl",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/bin/sh",
						"-c",
						"code=0; while [ $code != 200 ]; do code=$(curl -s -o /dev/null -w \"%{http_code}\" " + url + "); sleep 1; done; echo $code",
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	_, err := c.CoreV1().Pods(ns).Create(pod)
	if err != nil {
		return err
	}
	defer func() {
		utils.Logf("Deleting front pod")
		err = utils.DeletePod(c, ns, podName)
	}()

	// publicFlag shows whether public accessible test ends
	// internalFlag shows whether internal accessible test ends
	utils.Logf("Call from the created pod")
	err = wait.PollImmediate(pullInterval, pullTimeout, func() (bool, error) {
		pod, err := c.CoreV1().Pods(ns).Get(podName, metav1.GetOptions{})
		if err != nil {
			if utils.IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase != v1.PodSucceeded {
			utils.Logf("waiting for the pod succeeded")
			return false, nil
		}
		if pod.Status.ContainerStatuses[0].State.Terminated == nil || pod.Status.ContainerStatuses[0].State.Terminated.Reason != "Completed" {
			utils.Logf("waiting for the container completed")
			return false, nil
		}
		utils.Logf("Still testing internal access from front pod to internal service")
		log, err := c.CoreV1().Pods(ns).GetLogs(pod.Name, &v1.PodLogOptions{}).Do().Raw()
		if err != nil {
			return false, nil
		}
		return strings.Contains(fmt.Sprintf("%s", log), "200"), nil
	})
	utils.Logf("validation finished")
	return err
}

func validateLoadBalancerBackendPools(vmssname string, cs clientset.Interface, labels map[string]string, ns string, ports []v1.ServicePort, resourceGroupName string) {
	serviceName := "validate-lb-mode"
	lbModeLoadBalancerName := serviceName + string(uuid.NewUUID())
	// create deployment
	utils.Logf("Creating deployment " + lbModeLoadBalancerName)
	deployment := createNginxDeploymentManifest(lbModeLoadBalancerName, labels)
	_, err := cs.AppsV1().Deployments(ns).Create(deployment)
	Expect(err).NotTo(HaveOccurred())

	// create annotation for LoadBalancer service
	annotation := map[string]string{
		azure.ServiceAnnotationLoadBalancerMode: vmssname,
	}
	service := createLoadBalancerServiceManifest(cs, lbModeLoadBalancerName, annotation, labels, ns, ports)
	_, er := cs.CoreV1().Services(ns).Create(service)
	Expect(er).NotTo(HaveOccurred())
	utils.Logf("Successfully created LoadBalancer service " + lbModeLoadBalancerName + " in namespace " + ns)

	//wait and get service's public IP Address
	By("Waiting for service exposure")
	pip, err := utils.WaitServiceExposure(cs, ns, serviceName)
	Expect(err).NotTo(HaveOccurred())

	//Invoking azure network client to get list of public IP Addresses
	ctx := context.Background()
	azureTestClient, err := utils.CreateAzureTestClient()
	Expect(err).NotTo(HaveOccurred())
	iplist, er := azureTestClient.ListPublicIPs(ctx, resourceGroupName)
	Expect(er).NotTo(HaveOccurred())
	var pipID string
	for _, ip := range iplist {
		if ip.PublicIPAddressPropertiesFormat != nil && ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil && *ip.PublicIPAddressPropertiesFormat.IPAddress == pip {
			pipID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
			utils.Logf("Public Ip verified")
		}
	}
	//Get Azure loadBalancer Name
	match := lbNameRE.FindStringSubmatch(pipID)
	if len(match) != 2 {
		Skip("Report error")
	}
	loadBalancername := match[1]
	utils.Logf(loadBalancername)
	//Get backendpools list
	lbClient, err := azureTestClient.GetLoadBalancer()
	loadBalancerproperties, err := lbClient.ListComplete(ctx, loadBalancername)
	Expect(err).NotTo(HaveOccurred())
	Expect(loadBalancerproperties.Value().BackendAddressPools).NotTo(BeNil())
	Expect(loadBalancerproperties.Value().LoadBalancingRules).NotTo(BeNil())

	//Get the backendpoolID from loadbalancingrules and then verify the vmss name from backendIPConfiguration
	By("Validating loadBalancer backendPool")
	for _, pool := range *lb.BackendAddressPools {
		if pool.ID == nil || pool.BackendIPConfigurations == nil || !strings.EqualFold(*pool.ID, backendPoolID) {
			continue
		}
		for _, ipConfig := range *pool.BackendIPConfigurations {
			if ipConfig.ID == nil {
				continue
			}
			matches := backendIPConfigurationRE.FindStringSubmatch(*ipConfig.ID)
			Expect(len(matches)).To(Equal(2))
			Expect(matches[1]).To(Equal(vmssName))
		}
	}
}
