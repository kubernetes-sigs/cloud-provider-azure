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
	"k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	pauseImage = "k8s.gcr.io/pause:3.1"
)

// getPodList is a wapper around listing pods
func getPodList(cs clientset.Interface, ns string) (*v1.PodList, error) {
	var pods *v1.PodList
	var err error
	if wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pods, err = cs.CoreV1().Pods(ns).List(metav1.ListOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}) != nil {
		return pods, err
	}
	return pods, nil
}

// LogPodStatus logs the rate of pending
func LogPodStatus(cs clientset.Interface, ns string) error {
	pods, err := getPodList(cs, ns)
	if err != nil {
		return err
	}
	pendingPodCount := 0
	for _, p := range pods.Items {
		if p.Status.Phase == v1.PodPending {
			pendingPodCount++
		}
	}
	Logf("%d of %d pods in namespace %s are pending", pendingPodCount, len(pods.Items), ns)
	return nil
}

// DeletePodsInNamespace deletes all pods in the namespace
func DeletePodsInNamespace(cs clientset.Interface, ns string) error {
	Logf("Deleting all pods in namespace %s", ns)
	pods, err := getPodList(cs, ns)
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		err = DeletePod(cs, ns, p.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

// DeletePod deletes a single pod
func DeletePod(cs clientset.Interface, ns string, podName string) error {
	err := cs.CoreV1().Pods(ns).Delete(podName, nil)
	Logf("Deleting pod %s in namespace %s", podName, ns)
	if err != nil {
		return err
	}
	return wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
		if _, err := cs.CoreV1().Pods(ns).Get(podName, metav1.GetOptions{}); err != nil {
			return apierrs.IsNotFound(err), nil
		}
		return false, nil
	})
}
