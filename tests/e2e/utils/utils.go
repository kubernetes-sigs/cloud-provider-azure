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
	"fmt"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	deletionTimeout       = 10 * time.Minute
	poll                  = 2 * time.Second
	singleCallTimeout     = 20 * time.Minute
	vmssOperationInterval = 30 * time.Second
	vmssOperationTimeout  = 30 * time.Minute
)

func findExistingKubeConfig() string {
	// locations using DefaultClientConfigLoadingRules
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return rules.GetDefaultFilename()
}

// CreateKubeClientSet obtains the client set interface from Kubeconfig
func CreateKubeClientSet() (clientset.Interface, error) {
	//TODO: It should implement only once
	//rather than once per test
	Logf("Creating a kubernetes client")

	var (
		restConfig *rest.Config
		err        error
	)
	if envVarFiles := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); len(envVarFiles) != 0 {
		filename := findExistingKubeConfig()
		Logf("Kubernetes configuration file name: %s", filename)
		c := clientcmd.GetConfigFromFileOrDie(filename)
		restConfig, err = clientcmd.NewDefaultClientConfig(*c, &clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: ""}}).ClientConfig()
		if err != nil {
			return nil, err
		}
	} else {
		Logf("Cannot find %s env var, switch to use the in-cluster config", clientcmd.RecommendedConfigPathEnvVar)
		restConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	clientSet, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return clientSet, nil
}

// CreateTestingNamespace builds namespace for each test baseName and labels determine name of the space
func CreateTestingNamespace(baseName string, cs clientset.Interface) (*v1.Namespace, error) {
	namespaceObj := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("e2e-tests-%v-", baseName),
			Namespace:    "",
		},
		Status: v1.NamespaceStatus{},
	}
	// Be robust about making the namespace creation call.
	var got *v1.Namespace
	if err := wait.PollImmediate(poll, 30*time.Second, func() (bool, error) {
		var err error
		got, err = cs.CoreV1().Namespaces().Create(context.TODO(), namespaceObj, metav1.CreateOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	Logf("Created a test namespace %q", got.Name)
	return got, nil
}

func getNamespaceList(cs clientset.Interface) (*v1.NamespaceList, error) {
	var list *v1.NamespaceList
	if err := wait.PollImmediate(poll, 30*time.Second, func() (bool, error) {
		var err error
		list, err = cs.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteNamespace deletes the provided namespace, waits for it to be completely deleted, and then checks
// whether there are any pods remaining in a non-terminating state.
func DeleteNamespace(cs clientset.Interface, namespace string) error {
	Logf("Deleting namespace %s", namespace)
	if err := cs.CoreV1().Namespaces().Delete(context.TODO(), namespace, metav1.DeleteOptions{}); err != nil {
		return err
	}
	// wait for namespace to delete or timeout.
	err := wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
		if _, err := cs.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{}); err != nil {
			if apierrs.IsNotFound(err) {
				return true, nil
			}
			Logf("Error while waiting for namespace to be terminated: %w", err)
			if !IsRetryableAPIError(err) {
				return false, err
			}
		}
		return false, nil
	})
	return err
}

// IsRetryableAPIError will judge whether an error retryable or not
func IsRetryableAPIError(err error) bool {
	// These errors may indicate a transient error that we can retry in tests.
	if apierrs.IsInternalError(err) || apierrs.IsTimeout(err) || apierrs.IsServerTimeout(err) ||
		apierrs.IsTooManyRequests(err) || utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) {
		return true
	}
	// If the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := apierrs.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

// ExtractDNSPrefix obtains the cluster DNS prefix
func ExtractDNSPrefix() string {
	c := obtainConfig()
	parts := strings.Split(c.CurrentContext, "@")
	return parts[len(parts)-1]
}

// Load config from file
func obtainConfig() *clientcmdapi.Config {
	filename := findExistingKubeConfig()
	c := clientcmd.GetConfigFromFileOrDie(filename)
	return c
}

// StringInSlice check if string in a list
func StringInSlice(s string, list []string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// HandleVMNotFoundErr returns true if the input error is errVMNotFound or nil
func HandleVMNotFoundErr(err error) bool {
	return err == nil || err == errVMNotFound
}

// HandleVMSSNotFoundErr returns true if the input error is errVMSSNotFound or nil
func HandleVMSSNotFoundErr(err error) bool {
	return err == nil || err == errVMSSNotFound
}
