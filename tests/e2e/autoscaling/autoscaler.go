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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const podSize int64 = 200 //podSize to create, 200m, 0.2 core

// Cluster autoscaling cannot run in parallel, since multiple threads will infect the count of nodes
// It is slow, where at most 30 minutes are required to complete a single up or down scale
var _ = Describe("Cluster size autoscaler [Serial][Slow]", func() {
	basename := "autoscaler"

	var cs clientset.Interface
	var ns *v1.Namespace

	var initNodeCount int

	var podCount int // To make sure enough pods to exceed the temporary node volume

	var initNodesNames []string

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
		utils.Logf("Initial number of cores: %v", initNodeCapacity.Value())

		runningQuantity, err := utils.GetAvailableNodeCapacity(cs)
		Expect(err).NotTo(HaveOccurred())
		emptyQuantity := initNodeCapacity.Copy()
		emptyQuantity.Sub(runningQuantity)

		podCount = int(emptyQuantity.MilliValue()/podSize) + 1
		utils.Logf("%vm space are already in use", runningQuantity.Value())
		utils.Logf("will create %v pod, each %vm size", podCount, podSize)
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
		utils.Logf("Create deployment")
		replicas := int32(podCount)
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename})
		_, err := cs.AppsV1().Deployments(ns.Name).Create(deployment)
		Expect(err).NotTo(HaveOccurred())

		By("Scale up")
		targetNodeCount := initNodeCount + 1
		err = utils.WaitAutoScaleNodes(cs, targetNodeCount)
		utils.LogPodStatus(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Scale down")
		utils.Logf("Delete Pods by replic=0")
		replicas = 0
		deployment.Spec.Replicas = &replicas
		_, err = cs.AppsV1().Deployments(ns.Name).Update(deployment)
		Expect(err).NotTo(HaveOccurred())
		targetNodeCount = initNodeCount
		err = utils.WaitAutoScaleNodes(cs, targetNodeCount)
		utils.LogPodStatus(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
	})
})

func createPodSpec() (result v1.PodSpec) {
	result = v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:  "container",
				Image: "nginx:1.15",
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse(
							strconv.FormatInt(podSize, 10) + "m"),
					},
				},
			},
		},
	}
	return
}

func createScalerPodManifest(name string) (result *v1.Pod) {
	spec := createPodSpec()
	result = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: spec,
	}
	return
}

func createDeploymentManifest(name string, replicas int32, label map[string]string) (result *appsv1.Deployment) {
	spec := createPodSpec()
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

// stringInSlice check if string in a list
func stringInSlice(s string, list []string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
