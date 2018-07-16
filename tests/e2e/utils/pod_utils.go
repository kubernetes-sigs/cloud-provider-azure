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

// WaitDeletePods deletes the pods
func WaitDeletePods(cs clientset.Interface, ns string) error {
	pods, err := waitListPods(cs, ns)
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		cs.CoreV1().Pods(ns).Delete(p.Name, nil)
	}
	for _, p := range pods.Items {
		if err := wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
			if _, err := cs.CoreV1().Pods(ns).Get(p.Name, metav1.GetOptions{}); err != nil {
				return apierrs.IsNotFound(err), nil
			}
			return false, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// waitListPods is a wapper around listing pods
func waitListPods(cs clientset.Interface, ns string) (*v1.PodList, error) {
	var pods *v1.PodList
	var err error
	if wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pods, err = cs.CoreV1().Pods(ns).List(metav1.ListOptions{})
		if err != nil {
			if isRetryableAPIError(err) {
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
