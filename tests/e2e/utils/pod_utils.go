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
	"regexp"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	pollInterval = 10 * time.Second
	pollTimeout  = 5 * time.Minute
)

// PodIPRE tests if there's a valid IP in a easy way
var PodIPRE = regexp.MustCompile(`\d{0,3}\.\d{0,3}\.\d{0,3}\.\d{0,3}`)

// GetPodList is a wrapper around listing pods
func GetPodList(cs clientset.Interface, ns string) (*v1.PodList, error) {
	var pods *v1.PodList
	var err error
	if wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pods, err = cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{})
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

// countPendingPods counts how many pods is in the `pending` state
func countPendingPods(cs clientset.Interface, ns string, pods []v1.Pod) int {
	pendingPodCount := 0
	for _, p := range pods {
		if p.Status.Phase == v1.PodPending {
			pendingPodCount++
		}
	}

	return pendingPodCount
}

func WaitPodsToBeReady(cs clientset.Interface, ns string) error {
	var pods []v1.Pod
	err := wait.PollImmediate(pollInterval, pollTimeout, func() (done bool, err error) {
		podList, err := GetPodList(cs, ns)
		if err != nil {
			Logf("failed to list pods in namespace %s", ns)
			return false, err
		}
		pods = podList.Items
		pendingPodCount := countPendingPods(cs, ns, pods)
		if err != nil {
			Logf("unexpected error: %w", err)
			return false, err
		}

		Logf("%d pods in namespace %s are pending", pendingPodCount, ns)
		return pendingPodCount == 0, nil
	})
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) {
			for _, pod := range pods {
				printPodInfo(cs, ns, pod.Name)
			}
		}
		return err
	}

	return nil
}

// LogPodStatus logs the rate of pending
func LogPodStatus(cs clientset.Interface, ns string) error {
	podList, err := GetPodList(cs, ns)
	if err != nil {
		Logf("failed to list pods in namespace %s", ns)
		return err
	}

	pods := podList.Items
	pendingPodCount := countPendingPods(cs, ns, pods)
	if err != nil {
		return err
	}
	Logf("%d pods in namespace %s are pending", pendingPodCount, ns)
	return nil
}

// DeletePodsInNamespace deletes all pods in the namespace
func DeletePodsInNamespace(cs clientset.Interface, ns string) error {
	Logf("Deleting all pods in namespace %s", ns)
	pods, err := GetPodList(cs, ns)
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
	err := cs.CoreV1().Pods(ns).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	Logf("Deleting pod %s in namespace %s", podName, ns)
	if err != nil {
		if apierrs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return wait.PollImmediate(poll, deletionTimeout, func() (bool, error) {
		return !CheckPodExist(cs, ns, podName), nil
	})
}

// CreatePod creates a new pod
func CreatePod(cs clientset.Interface, ns string, manifest *v1.Pod) error {
	Logf("creating pod %s in namespace %s", manifest.Name, ns)
	_, err := cs.CoreV1().Pods(ns).Create(context.TODO(), manifest, metav1.CreateOptions{})
	return err
}

// CreateHostExecPod creates an Agnhost Pod to exec. It returns if the Pod is running and error.
func CreateHostExecPod(cs clientset.Interface, ns, name string) (bool, error) {
	Logf("Creating a hostNetwork Pod %s in namespace %s to exec", name, ns)
	immediate := int64(0)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "agnhost",
					Image:           "registry.k8s.io/e2e-test-images/agnhost:2.36",
					Args:            []string{"pause"},
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
			HostNetwork:                   true,
			TerminationGracePeriodSeconds: &immediate,
			// Set the Pod to control plane Node because it is a Linux one
			Tolerations: []v1.Toleration{
				{
					Key:      controlPlaneNodeRoleLabel,
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
				{
					Key:      masterNodeRoleLabel,
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
			},
			NodeSelector: map[string]string{
				nodeOSLabel: "linux",
			},
		},
	}
	if err := CreatePod(cs, ns, pod); err != nil {
		return false, err
	}
	return WaitPodTo(v1.PodRunning, cs, pod, ns)
}

// GetPod returns a Pod with namespace and name.
func GetPod(cs clientset.Interface, ns, name string) (pod *v1.Pod, err error) {
	if pollErr := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		pod, err = cs.CoreV1().Pods(ns).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			if apierrs.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); errors.Is(pollErr, wait.ErrWaitTimeout) {
		return nil, fmt.Errorf("failed to get Pod %q in namespace %q", name, ns)
	}
	return pod, err
}

// CheckPodExist checks if a Pod exists in a namespace with its name.
func CheckPodExist(cs clientset.Interface, ns, name string) bool {
	_, err := cs.CoreV1().Pods(ns).Get(context.TODO(), name, metav1.GetOptions{})
	return !apierrs.IsNotFound(err)
}

// getPodLogs gets the log of the given pods
func getPodLogs(cs clientset.Interface, ns, podName string, opts *v1.PodLogOptions) ([]byte, error) {
	Logf("getting the log of pod %s", podName)
	return cs.CoreV1().Pods(ns).GetLogs(podName, opts).Do(context.TODO()).Raw()
}

// printPodInfo prints Pod describe info and logs.
func printPodInfo(cs clientset.Interface, ns, pod string) {
	output, _ := RunKubectl(ns, "describe", "pod", pod)
	Logf("Describe info of Pod %q:\n%s", pod, output)
	log, _ := getPodLogs(cs, ns, pod, &v1.PodLogOptions{})
	Logf("Log of Pod %q:\n%s", pod, log)
	log, _ = getPodLogs(cs, ns, pod, &v1.PodLogOptions{Previous: true})
	Logf("Previous log of Pod %q:\n%s", pod, log)
}

// CreatePodGetIPManifest creates a Pod manifest getting IP with ifconfig.me.
func CreatePodGetIPManifest() *v1.Pod {
	podName := "test-pod"
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Hostname: podName,
			Containers: []v1.Container{
				{
					Name:            "test-app",
					Image:           "registry.k8s.io/e2e-test-images/agnhost:2.36",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/bin/sh", "-c", "curl -s -m 5 --retry-delay 5 --retry 10 ifconfig.me",
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}

// GetPodOutboundIP returns the outbound IP of the given pod.
// It must be used with utils.CreatePodGetIPManifest()
func GetPodOutboundIP(cs clientset.Interface, podTemplate *v1.Pod, nsName string) (string, error) {
	var log []byte
	err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		pod, err := cs.CoreV1().Pods(nsName).Get(context.TODO(), podTemplate.Name, metav1.GetOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase != v1.PodSucceeded {
			Logf("waiting for the pod to succeed, current status: %s", pod.Status.Phase)
			if pod.Status.Phase == v1.PodFailed {
				return false, wait.ErrWaitTimeout
			}
			return false, nil
		}
		if pod.Status.ContainerStatuses[0].State.Terminated == nil || pod.Status.ContainerStatuses[0].State.Terminated.Reason != "Completed" {
			Logf("waiting for the container to be completed")
			return false, nil
		}
		log, err = getPodLogs(cs, nsName, podTemplate.Name, &v1.PodLogOptions{})
		if err != nil {
			Logf("retrying getting pod's log")
			return false, nil
		}
		return PodIPRE.MatchString(string(log)), nil
	})
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) {
			printPodInfo(cs, nsName, podTemplate.Name)
		}
		return "", err
	}
	ip := PodIPRE.FindString(string(log))
	Logf("Got pod outbound IP %s", ip)
	return ip, nil
}

// WaitPodTo returns True if pod is in the specific phase during
// a short period of time
func WaitPodTo(phase v1.PodPhase, cs clientset.Interface, podTemplate *v1.Pod, nsName string) (result bool, err error) {
	if err := wait.PollImmediate(pollInterval, pollTimeout, func() (result bool, err error) {
		pod, err := cs.CoreV1().Pods(nsName).Get(context.TODO(), podTemplate.Name, metav1.GetOptions{})
		if err != nil {
			if IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase != phase {
			Logf("waiting for the pod status to be %s, current status: %s", phase, pod.Status.Phase)
			return false, nil
		}
		return true, nil
	}); err != nil {
		printPodInfo(cs, nsName, podTemplate.Name)
		return false, err
	}
	return true, err
}
