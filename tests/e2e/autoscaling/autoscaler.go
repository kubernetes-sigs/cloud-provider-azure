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

package autoscaling

import (
	"context"
	"math"
	"os"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	podSize     int64 = 200 //podSize to create, 200m, 0.2 core
	poll              = 10 * time.Second
	pollTimeout       = 10 * time.Minute
	execTimeout       = 10 * time.Second
)

// Cluster autoscaling cannot run in parallel, since multiple threads will infect the count of nodes
// It is slow, where at most 30 minutes are required to complete a single up or down scale
var _ = Describe("Cluster size autoscaler [Serial][Slow]", func() {
	var (
		basename       = "autoscaler"
		cs             clientset.Interface
		ns             *v1.Namespace
		initNodeCount  int
		podCount       int // To make sure enough pods to exceed the temporary node volume
		initNodesNames []string
	)

	testContext := &framework.TestContext
	// Use kubectl binary in $PATH
	testContext.KubectlPath = "kubectl"
	testContext.KubeConfig = os.Getenv("KUBECONFIG")
	framework.AfterReadingAllFlags(testContext)

	BeforeEach(func() {
		var err error
		By("Create test context")
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		initNodeCount = len(nodes)
		utils.Logf("Initial number of schedulable nodes: %v", initNodeCount)

		// TODO:
		// There should be a judgement that the initNodeCount should be smaller than the max nodes property
		// of the cluster, otherwise skip the test

		var initNodeCapacity resource.Quantity
		for _, node := range nodes {
			initNodesNames = append(initNodesNames, node.Name)
			quantity := node.Status.Capacity[v1.ResourceCPU]
			initNodeCapacity.Add(quantity)
		}
		utils.Logf("Initial number of cores: %vm", initNodeCapacity.MilliValue())

		runningQuantity, err := utils.GetAvailableNodeCapacity(cs, initNodesNames)
		Expect(err).NotTo(HaveOccurred())
		emptyQuantity := initNodeCapacity.DeepCopy()
		emptyQuantity.Sub(runningQuantity)

		// Calculate the number of pods needed in a deployment to trigger a scale up operation
		podCount = int(math.Ceil(float64(emptyQuantity.MilliValue())/float64(podSize))) + 1
		utils.Logf("%vm space are already in use", runningQuantity.MilliValue())
		utils.Logf("will create %v pods in a deployment, each pod requests %vm size", podCount, podSize)
	})

	AfterEach(func() {
		err := utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		//delete extra nodes
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		var nodesToDelete []string
		for _, n := range nodes {
			if !stringInSlice(n.Name, initNodesNames) {
				nodesToDelete = append(nodesToDelete, n.Name)
			}
		}
		err = utils.DeleteNodes(cs, nodesToDelete)
		Expect(err).NotTo(HaveOccurred())

		// clean up
		cs = nil
		initNodesNames = nil
		ns = nil
		initNodeCount = 0
		podCount = 0
	})

	It("should scale up or down if deployment replicas leave nodes busy or idle [Feature:Autoscaling]", func() {
		utils.Logf("Create a deployment that will trigger a scale up operation")
		replicas := int32(podCount)
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename})
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		waitForScaleUpToComplete(cs, ns, initNodeCount)
		waitForScaleDownToComplete(cs, ns, initNodeCount, deployment)
	})

	It("should scale up, deploy a statefulset with disks attached, scale down, and certain pods + disks should be evicted to a new node [Feature:Autoscaling]", func() {
		By("Creating a deployment that will trigger a scale up operation")
		replicas := int32(podCount)
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename + "-deployment"})
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitForScaleUpToComplete(cs, ns, initNodeCount)

		By("Deploying a StatefulSet")
		statefulSetManifest := createStatefulSetWithPVCManifest(basename+"-statefulset", int32(5), map[string]string{"app": basename + "-statefulset"})
		statefulSet, err := cs.AppsV1().StatefulSets(ns.Name).Create(context.TODO(), statefulSetManifest, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the StatefulSet to become ready")
		err = waitForStatefulSetComplete(cs, ns, statefulSet)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying mounted volumes in each StatefulSet's pod")
		// A map that keeps track of which node the pod is scheduled to
		podNodeMap := make(map[string]string)
		selector, err := metav1.LabelSelectorAsSelector(statefulSet.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		options := metav1.ListOptions{LabelSelector: selector.String()}
		statefulSetPods, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), options)
		Expect(err).NotTo(HaveOccurred())
		for _, pod := range statefulSetPods.Items {
			podNodeMap[pod.Name] = pod.Spec.NodeName
			_, err := framework.LookForStringInPodExec(ns.Name, pod.Name, []string{"cat", "/mnt/test/data"}, "hello world", execTimeout)
			Expect(err).NotTo(HaveOccurred())
		}

		waitForScaleDownToComplete(cs, ns, initNodeCount, deployment)

		By("Waiting for certain StatefulSet's pods + disks to be evicted to new nodes")
		err = waitForStatefulSetComplete(cs, ns, statefulSet)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying mounted volumes in each StatefulSet's pod after scaling down")
		statefulSetPods, err = cs.CoreV1().Pods(ns.Name).List(context.TODO(), options)
		Expect(err).NotTo(HaveOccurred())
		for _, pod := range statefulSetPods.Items {
			var expectedOutput string
			if nodeName, ok := podNodeMap[pod.Name]; !ok || nodeName != pod.Spec.NodeName {
				// pod got evicted to a different node after scaling down
				// so it should have two 'hello world' string in data file
				expectedOutput = "hello world\nhello world"
			} else {
				// If pod did not get re-scheduled, data file should remain the same
				expectedOutput = "hello world"
			}
			_, err := framework.LookForStringInPodExec(ns.Name, pod.Name, []string{"cat", "/mnt/test/data"}, expectedOutput, execTimeout)
			Expect(err).NotTo(HaveOccurred())
		}
	})
})

func waitForScaleUpToComplete(cs clientset.Interface, ns *v1.Namespace, initNodeCount int) {
	By("Waiting for a scale up operation to happen")
	targetNodeCount := initNodeCount + 1
	err := utils.WaitAutoScaleNodes(cs, targetNodeCount, false)
	Expect(err).NotTo(HaveOccurred())
	err = utils.LogPodStatus(cs, ns.Name)
	Expect(err).NotTo(HaveOccurred())
}

func waitForScaleDownToComplete(cs clientset.Interface, ns *v1.Namespace, initNodeCount int, deployment *appsv1.Deployment) {
	By("Waiting for a scale down operation to happen")
	utils.Logf("Delete pods by setting the number of replicas in the deployment to 0")
	replicas := int32(0)
	deployment.Spec.Replicas = &replicas
	_, err := cs.AppsV1().Deployments(ns.Name).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	targetNodeCount := initNodeCount
	err = utils.WaitAutoScaleNodes(cs, targetNodeCount, true)
	Expect(err).NotTo(HaveOccurred())
	err = utils.LogPodStatus(cs, ns.Name)
	Expect(err).NotTo(HaveOccurred())
}

func createPodSpec(requestedCPU int64) (result v1.PodSpec) {
	result = v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:  "container",
				Image: "nginx:1.15",
			},
		},
	}
	if requestedCPU != 0 {
		result.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse(
					strconv.FormatInt(requestedCPU, 10) + "m"),
			},
		}
	}
	return
}

func createDeploymentManifest(name string, replicas int32, label map[string]string) (result *appsv1.Deployment) {
	spec := createPodSpec(podSize)
	result = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: label,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: label,
				},
				Spec: spec,
			},
		},
	}
	return
}

func createStatefulSetWithPVCManifest(name string, replicas int32, label map[string]string) *appsv1.StatefulSet {
	pvcName := "pvc"
	spec := createPodSpec(0)
	spec.Containers[0].Command = []string{"/bin/sh"}
	spec.Containers[0].Args = []string{"-c", "echo 'hello world' >> /mnt/test/data && while true; do sleep 1; done"}
	spec.Containers[0].VolumeMounts = []v1.VolumeMount{
		{
			Name:      pvcName,
			MountPath: "/mnt/test",
		},
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: label,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: label,
					Annotations: map[string]string{
						"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
					},
				},
				Spec: spec,
			},
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: pvcName,
					},
					Spec: v1.PersistentVolumeClaimSpec{
						AccessModes: []v1.PersistentVolumeAccessMode{
							v1.ReadWriteOnce,
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceStorage): resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}
}

func waitForStatefulSetComplete(cs clientset.Interface, ns *v1.Namespace, ss *appsv1.StatefulSet) error {
	err := wait.PollImmediate(poll, pollTimeout, func() (bool, error) {
		var err error
		statefulSet, err := cs.AppsV1().StatefulSets(ns.Name).Get(context.TODO(), ss.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		utils.Logf("%d/%d replicas in the StatefulSet are ready", statefulSet.Status.ReadyReplicas, *statefulSet.Spec.Replicas)
		if statefulSet.Status.ReadyReplicas == *statefulSet.Spec.Replicas {
			return true, nil
		}
		return false, nil
	})

	return err
}

// stringInSlice check if string in a list
func stringInSlice(s string, list []string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
