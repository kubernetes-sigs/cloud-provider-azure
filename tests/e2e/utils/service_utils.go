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

package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	. "github.com/onsi/gomega"
)

const (
	serviceTimeout        = 5 * time.Minute
	serviceTimeoutBasicLB = 10 * time.Minute
	pullInterval          = 10 * time.Second
	pullTimeout           = 3 * time.Minute

	ExecAgnhostPod = "exec-agnhost-pod"
)

// DeleteService deletes a service if it exists, return nil if not exists.
func DeleteService(cs clientset.Interface, ns string, serviceName string) error {
	Logf("Deleting service %q in namespace %q", serviceName, ns)
	if err := cs.CoreV1().Services(ns).Delete(context.TODO(), serviceName, metav1.DeleteOptions{}); err != nil {
		if apierrs.IsNotFound(err) {
			Logf("Service %q does not exist, no need to delete", serviceName)
			return nil
		}
		output, _ := RunKubectl(ns, "describe", "service", serviceName)
		Logf("Service describe info: %s", output)
		return err
	}
	err := wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
		if _, err := cs.CoreV1().Services(ns).Get(context.TODO(), serviceName, metav1.GetOptions{}); err != nil {
			return apierrs.IsNotFound(err), nil
		}
		return false, nil
	})
	if err != nil {
		output, _ := RunKubectl(ns, "describe", "service", serviceName)
		Logf("Service describe info: %s", output)
	}
	return err
}

// GetServiceDomainName cat prefix and azure suffix
func GetServiceDomainName(prefix string) (ret string) {
	suffix := extractSuffix()
	if os.Getenv(AKSTestCCM) != "" {
		suffix = "." + os.Getenv(ClusterLocationEnv) + ".cloudapp.azure.com"
	}
	ret = prefix + suffix
	Logf("Get domain name: %s", ret)
	return
}

// WaitServiceExposureAndGetIPs returns IPs of the Service.
func WaitServiceExposureAndGetIPs(cs clientset.Interface, namespace string, name string) ([]*string, error) {
	var service *v1.Service
	var err error
	ips := []*string{}

	service, err = WaitServiceExposure(cs, namespace, name, []*string{})
	if err != nil {
		return ips, err
	}
	if service == nil {
		return ips, errors.New("the service is nil")
	}

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ingress := ingress
		ips = append(ips, &ingress.IP)
	}

	return ips, nil
}

// WaitServiceExposureAndValidateConnectivity returns IPs of the service and check the connectivity if they are public IPs.
// Service should have been created before calling this function.
func WaitServiceExposureAndValidateConnectivity(cs clientset.Interface, ipFamily IPFamily, namespace string, name string, targetIPs []*string) ([]*string, error) {
	var service *v1.Service
	var err error
	var ips []*string

	defer func() {
		if err != nil {
			deleteSvcErr := DeleteService(cs, namespace, name)
			Expect(deleteSvcErr).NotTo(HaveOccurred())
		}
	}()

	service, err = WaitServiceExposure(cs, namespace, name, targetIPs)
	if err != nil {
		return ips, err
	}
	if service == nil {
		err = errors.New("the service is nil")
		return ips, err
	}

	if len(service.Status.LoadBalancer.Ingress) == 0 {
		err = errors.New("service.Status.LoadBalancer.Ingress is empty")
		return ips, err
	}
	ips = append(ips, &service.Status.LoadBalancer.Ingress[0].IP)
	if len(service.Status.LoadBalancer.Ingress) > 1 {
		ips = append(ips, &service.Status.LoadBalancer.Ingress[1].IP)
	}

	// Create host exec Pod
	result, err := CreateHostExecPod(cs, namespace, ExecAgnhostPod)
	defer func() {
		deletePodErr := DeletePod(cs, namespace, ExecAgnhostPod)
		if deletePodErr != nil {
			Logf("failed to delete ExecAgnhostPod, error: %v", err)
		}
	}()
	if !result || err != nil {
		err = fmt.Errorf("failed to create ExecAgnhostPod, result: %v, error: %w", result, err)
		return ips, err
	}

	// TODO: Check if other WaitServiceExposureAndValidateConnectivity() callers with internal Service
	// should test connectivity as well.
	for _, port := range service.Spec.Ports {
		for _, ip := range ips {
			Logf("checking the connectivity of address %q %d with protocol %v", *ip, int(port.Port), port.Protocol)
			if err := ValidateServiceConnectivity(namespace, ExecAgnhostPod, *ip, int(port.Port), port.Protocol); err != nil {
				return ips, err
			}
		}
	}

	return ips, nil
}

// WaitServiceExposure waits for the exposure of the external IP of the service
func WaitServiceExposure(cs clientset.Interface, namespace string, name string, targetIPs []*string) (*v1.Service, error) {
	var service *v1.Service
	var err error
	var externalIPs []*string

	timeout := serviceTimeout
	if skuEnv := os.Getenv(LoadBalancerSkuEnv); skuEnv != "" {
		if strings.EqualFold(skuEnv, string(aznetwork.LoadBalancerSkuNameBasic)) {
			timeout = serviceTimeoutBasicLB
		}
	}

	if err := wait.PollImmediate(10*time.Second, timeout, func() (bool, error) {
		service, err = cs.CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}

		IngressList := service.Status.LoadBalancer.Ingress
		if (len(targetIPs) != 0 && len(IngressList) < len(targetIPs)) ||
			(len(targetIPs) == 0 && len(IngressList) == 0) {
			err = fmt.Errorf("cannot find Ingress in limited time")
			Logf("Fail to find ingress, retry in 10 seconds")
			return false, nil
		}

		externalIPs = []*string{}
		for _, ingress := range IngressList {
			ingress := ingress
			externalIPs = append(externalIPs, &ingress.IP)
		}
		if len(targetIPs) != 0 {
			if !CompareStrings(externalIPs, targetIPs) {
				Logf("expected IPs are %v, current IPs are %v, retry in 10 seconds", StrPtrSliceToStrSlice(targetIPs), StrPtrSliceToStrSlice(externalIPs))
				return false, nil
			}
		}

		return true, nil
	}); err != nil {
		output, _ := RunKubectl(namespace, "describe", "service", name)
		Logf("Service describe info: %s", output)
		return nil, err
	}

	Logf("Exposure successfully, get external IPs: %v", StrPtrSliceToStrSlice(externalIPs))
	return service, nil
}

// ValidateServiceConnectivity validates the connectivity of the internal Service IP
func ValidateServiceConnectivity(ns, execPod, serviceIP string, port int, protocol v1.Protocol) error {
	udpOption := ""
	if protocol == v1.ProtocolUDP {
		udpOption = "-u"
	}
	cmd := fmt.Sprintf(`nc -vz -w 4 %s %s %d`, udpOption, serviceIP, port)
	pollErr := wait.PollImmediate(pullInterval, 3*pullTimeout, func() (bool, error) {
		stdout, err := RunKubectl(ns, "exec", execPod, "--", "/bin/sh", "-x", "-c", cmd)
		if err != nil {
			Logf("got error %v, will retry, output: %s", err, stdout)
			return false, nil
		}
		if !strings.Contains(stdout, "succeeded") {
			Logf("Expected output to contain 'succeeded', got %q; retrying...", stdout)
			return false, nil
		}
		Logf("Validation succeeded: Service addr %s and port %d with protocol %v", serviceIP, port, protocol)
		return true, nil
	})
	return pollErr
}

// extractSuffix obtains the server domain name suffix
func extractSuffix() string {
	c := obtainConfig()
	prefix := ExtractDNSPrefix()
	url := c.Clusters[prefix].Server
	suffix := url[strings.Index(url, "."):]
	if strings.Contains(suffix, ":") {
		suffix = suffix[:strings.Index(suffix, ":")]
	}
	return suffix
}

func IsInternalEndpoint(ip string) bool {
	return strings.HasPrefix(ip, "10.")
}
