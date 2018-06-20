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
	"fmt"
	"time"

	"k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	nodeLabelRole = "kubernetes.io/role"
)

func isRetryableAPIError(err error) bool {
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

// GetAgentNodes obtians the list of agent nodes
func GetAgentNodes(cs clientset.Interface) ([]v1.Node, error) {
	nodesList, err := waitListNodes(cs)
	if err != nil {
		return nil, err
	}
	ret := make([]v1.Node, 0)
	for _, node := range nodesList.Items {
		if !isMasterNode(&node) {
			ret = append(ret, node)
		}
	}
	return ret, nil
}

// waitListSchedulableNodes is a wapper around listing nodes
func waitListNodes(cs clientset.Interface) (*v1.NodeList, error) {
	var nodes *v1.NodeList
	var err error
	if wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		nodes, err = cs.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}) != nil {
		return nodes, err
	}
	return nodes, nil
}

// WaitAutoScaleNodes returns nodes count after autoscaling in 30 minutes
func WaitAutoScaleNodes(cs clientset.Interface, targetNodeCount int) error {
	Logf(fmt.Sprintf("waiting for auto-scaling the node... Target node count: %v", targetNodeCount))
	var nodes []v1.Node
	var err error
	poll := 20 * time.Second
	autoScaleTimeOut := 30 * time.Minute
	if wait.PollImmediate(poll, autoScaleTimeOut, func() (bool, error) {
		nodes, err = GetAgentNodes(cs)
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if nodes == nil {
			err = fmt.Errorf("Unexpected nil node list")
			return false, err
		}
		Logf("Detect %v nodes, target %v", len(nodes), targetNodeCount)
		return targetNodeCount == len(nodes), nil
	}) == wait.ErrWaitTimeout {
		return fmt.Errorf("Fail to get target node count in limited time")
	} else {
		return err
	}
}

// WaitNodeDeletion ensures a list of nodes to be deleted
func WaitNodeDeletion(cs clientset.Interface, names []string) error {
	for _, name := range names {
		if err := deleteNode(cs, name); err != nil {
			return err
		}
	}
	return nil
}

//deleteNodes deletes nodes according to names
func deleteNode(cs clientset.Interface, name string) error {
	Logf("Deleting node: %s", name)
	if err := cs.CoreV1().Nodes().Delete(name, nil); err != nil {
		return err
	}

	// wait for node to delete or timeout.
	err := wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
		if _, err := cs.CoreV1().Nodes().Get(name, metav1.GetOptions{}); err != nil {
			return apierrs.IsNotFound(err), nil
		}
		return false, nil
	})
	return err
}

// isMasterNode returns true if the node has a master role label.
// The master role is determined by looking for:
// * a kubernetes.io/role="master" label
func isMasterNode(node *v1.Node) bool {
	if val, ok := node.Labels[nodeLabelRole]; ok && val == "master" {
		return true
	}
	return false
}

func obtainMasterNodeNames(cs clientset.Interface) ([]string, error) {
	masters := make([]string, 0)
	nodeList, err := waitListNodes(cs)
	if err != nil {
		return masters, err
	}
	for _, node := range nodeList.Items {
		if isMasterNode(&node) {
			masters = append(masters, node.Name)
		}
	}
	return masters, nil
}

// GetAvailableNodeCapacity will calculate the overall quantity of
// cpu requested by all running pods in all namespaces
func GetAvailableNodeCapacity(cs clientset.Interface) (resource.Quantity, error) {
	var result resource.Quantity
	namespaceList, err := waitListNamespace(cs)
	if err != nil {
		return result, err
	}
	masterNodeNames, err := obtainMasterNodeNames(cs)
	if err != nil {
		return result, err
	}

	for _, namespace := range namespaceList.Items {
		podList, err := waitListPods(cs, namespace.Name)
		if err != nil {
			// will not abort, just ignore this namespace
			Logf("Ignore pods resource request in namespace %s", namespace.Name)
			continue
		}
		for _, pod := range podList.Items {
			if pod.Status.Phase == v1.PodRunning {
				if !stringInSlice(pod.Spec.NodeName, masterNodeNames) {
					for _, container := range pod.Spec.Containers {
						cpuRequest := container.Resources.Requests[v1.ResourceCPU]
						result.Add(cpuRequest)
					}
				}
			}
		}
	}
	return result, nil
}
