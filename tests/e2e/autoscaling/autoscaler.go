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
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	podSize            int64 = 200 // podSize to create, 200m, 0.2 core
	poll                     = 10 * time.Second
	statefulSetTimeout       = 30 * time.Minute
	execTimeout              = 10 * time.Second

	kubeSystemNamespace             = "kube-system"
	caDeploymentName                = "cluster-autoscaler"
	balanceNodeGroupsFlag           = "--balance-similar-node-groups=true"
	clusterAutoscalerDeploymentName = "cluster-autoscaler"
)

// Cluster autoscaling cannot run in parallel, since multiple threads will infect the count of nodes
// It is slow, where at most 30 minutes are required to complete a single up or down scale
var _ = Describe("Cluster size autoscaler", Label(utils.TestSuiteLabelFeatureAutoscaling, utils.TestSuiteLabelSerial, utils.TestSuiteLabelSlow), func() {
	var (
		basename            = "autoscaler"
		cs                  clientset.Interface
		ns                  *v1.Namespace
		tc                  *utils.AzureTestClient
		initNodeCount       int
		podCount            int32 // To make sure enough pods to exceed the temporary node volume
		nodes               []v1.Node
		initNodepoolNodeMap map[string][]string
	)

	BeforeEach(func() {
		var err error
		By("Create test context")
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		if os.Getenv(utils.AKSTestCCM) == "" || !strings.Contains(os.Getenv(utils.AKSClusterType), "autoscaling") {
			ready, err := isCAEnabled(cs)
			if err != nil {
				Skip(fmt.Sprintf("unexpected error %v occurs when getting cluster autoscaler deployment", err))
			}
			Expect(ready).To(BeTrue())
		}

		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		nodes, err = utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		initNodeCount = len(nodes)
		utils.Logf("Initial number of schedulable nodes: %v", initNodeCount)

		initNodepoolNodeMap = utils.GetNodepoolNodeMap(&nodes)
		utils.Logf("found %d node pools", len(initNodepoolNodeMap))

		// TODO:
		// There should be a judgement that the initNodeCount should be smaller than the max nodes property
		// of the cluster, otherwise skip the test

		for i := range nodes {
			podCount += calculateNewPodCountOnNode(cs, &nodes[i])
		}
		utils.Logf("create %d new pods will saturate the space", podCount)
	})

	AfterEach(func() {
		err := utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		//delete extra nodes
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		nodesNotToBeDeleted := make([]string, 0)
		for _, nodes := range initNodepoolNodeMap {
			nodesNotToBeDeleted = append(nodesNotToBeDeleted, nodes...)
		}

		nodesToBeDeleted := make([]string, 0)
		nodeToBeDeletedCount := 0
		if len(nodes)-initNodeCount > 0 {
			nodeToBeDeletedCount = len(nodes) - initNodeCount
		}

		for _, node := range nodes {
			if nodeToBeDeletedCount == 0 {
				break
			}

			if !utils.StringInSlice(node.Name, nodesNotToBeDeleted) {
				nodesToBeDeleted = append(nodesToBeDeleted, node.Name)
				nodeToBeDeletedCount--
			}
		}

		err = utils.DeleteNodes(cs, nodesToBeDeleted)
		Expect(err).NotTo(HaveOccurred())

		// clean up
		cs = nil
		ns = nil
		initNodeCount = 0
		podCount = 0
		initNodepoolNodeMap = nil
	})

	It("should scale up or down if deployment replicas leave nodes busy or idle", func() {
		utils.Logf("Create a deployment that will trigger a scale up operation")
		replicas := podCount + 1
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename}, podSize, false)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		waitForScaleUpToComplete(cs, ns, initNodeCount+1)
		waitForScaleDownToComplete(cs, ns, initNodeCount, deployment)
	})

	It("should scale up, deploy a statefulset with disks attached, scale down, and certain pods + disks should be evicted to a new node", func() {
		By("Creating a deployment that will trigger a scale up operation")
		replicas := podCount + 1
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename + "-deployment"}, podSize, false)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitForScaleUpToComplete(cs, ns, initNodeCount+1)

		By("Deploying a StatefulSet")
		statefulSetManifest := createStatefulSetWithPVCManifest(basename+"-statefulset", int32(2), map[string]string{"app": basename + "-statefulset"})
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
			_, err := utils.LookForStringInPodExec(ns.Name, pod.Name, []string{"cat", "/mnt/test/data"}, "hello world", execTimeout)
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
			_, err := utils.LookForStringInPodExec(ns.Name, pod.Name, []string{"cat", "/mnt/test/data"}, expectedOutput, execTimeout)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("should balance the sizes of multiple node group if the `--balance-node-groups` is set to true", Label(utils.TestSuiteLabelMultiNodePools), func() {
		By("Checking the number of node pools")
		if len(initNodepoolNodeMap) < 2 {
			Skip("multiple node pools are needed in this scenario")
		}

		By("Checking the `--balance-node-groups flag`")
		if os.Getenv(utils.AKSTestCCM) == "" || !strings.EqualFold(os.Getenv(utils.AKSClusterType), "autoscaling-multipool") {
			caDeployment, err := cs.AppsV1().Deployments(kubeSystemNamespace).Get(context.TODO(), caDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			commands := caDeployment.Spec.Template.Spec.Containers[0].Command
			if !utils.StringInSlice(balanceNodeGroupsFlag, commands) {
				Skip("`--balance-similar-node-groups=true` needs to be set in this scenario")
			}
		}

		By("Saturating the free space")
		deployment := createDeploymentManifest(basename+"-deployment", podCount, map[string]string{"app": basename + "-deployment"}, podSize, false)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Scaling out 10 new nodes")
		cpu := nodes[0].Status.Capacity[v1.ResourceCPU]
		scaleUpPodSize := int64(float64(cpu.MilliValue()) / 1.8)
		scaleUpDeployment := createDeploymentManifest(basename+"-deployment1", 10, map[string]string{"app": basename + "-deployment1"}, scaleUpPodSize, false)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), scaleUpDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitForScaleUpToComplete(cs, ns, len(nodes)+10)

		By("Checking the balancing state of the node groups")
		nodes, err = utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		isBalance := checkNodeGroupsBalance(&nodes)
		Expect(isBalance).To(BeTrue())

		waitForScaleDownToComplete(cs, ns, initNodeCount, scaleUpDeployment)
	})

	It("should support one node pool with slow scaling", Label(utils.TestSuiteLabelSingleNodePool), func() {
		By("Checking the number of node pools")
		if len(initNodepoolNodeMap) != 1 {
			Skip("single node pool is needed in this scenario")
		}

		By("Saturating the free space")
		deployment := createDeploymentManifest(basename+"-deployment", podCount, map[string]string{"app": basename + "-deployment"}, podSize, false)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Scaling up, 5 nodes at a time until reaches 50 nodes (40 nodes for aks cluster)")
		cpu := nodes[0].Status.Capacity[v1.ResourceCPU]
		scaleUpPodSize := int64(float64(cpu.MilliValue()) / 1.8)
		scaleUpDeployment := createDeploymentManifest(basename+"-deployment1", 0, map[string]string{"app": basename + "-deployment1"}, scaleUpPodSize, false)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), scaleUpDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		targetNodeCount := initNodeCount
		expectedNode := 50
		if os.Getenv(utils.AKSTestCCM) != "" {
			expectedNode = 40
		}
		for {
			*scaleUpDeployment.Spec.Replicas += 5
			targetNodeCount += 5
			_, err = cs.AppsV1().Deployments(ns.Name).Update(context.TODO(), scaleUpDeployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			waitForScaleUpToComplete(cs, ns, targetNodeCount)

			nodes, err := utils.GetAgentNodes(cs)
			Expect(err).NotTo(HaveOccurred())
			if len(nodes) > expectedNode {
				break
			}
		}

		waitForScaleDownToComplete(cs, ns, initNodeCount, scaleUpDeployment)
	})

	It("should support multiple node pools with quick scaling", Label(utils.TestSuiteLabelMultiNodePools), func() {
		By("Checking the number of node pools")
		if len(initNodepoolNodeMap) < 2 {
			Skip("multiple node pools are needed in this scenario")
		}

		By("Saturating the free space")
		deployment := createDeploymentManifest(basename+"-deployment", podCount, map[string]string{"app": basename + "-deployment"}, podSize, false)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Scaling up, 25 nodes at a time until reaches 50 nodes (38 nodes for aks cluster)")
		cpu := nodes[0].Status.Capacity[v1.ResourceCPU]
		scaleUpPodSize := int64(float64(cpu.MilliValue()) / 1.8)
		scaleUpDeployment := createDeploymentManifest(basename+"-deployment1", 0, map[string]string{"app": basename + "-deployment1"}, scaleUpPodSize, false)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), scaleUpDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		targetNodeCount := initNodeCount
		expectedNode := 50
		if os.Getenv(utils.AKSTestCCM) != "" {
			expectedNode = 38
		}
		for {
			*scaleUpDeployment.Spec.Replicas += int32(expectedNode / 2)
			targetNodeCount += expectedNode / 2
			_, err = cs.AppsV1().Deployments(ns.Name).Update(context.TODO(), scaleUpDeployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			waitForScaleUpToComplete(cs, ns, targetNodeCount)

			nodes, err := utils.GetAgentNodes(cs)
			Expect(err).NotTo(HaveOccurred())
			if len(nodes) > expectedNode {
				break
			}
		}

		waitForScaleDownToComplete(cs, ns, initNodeCount, scaleUpDeployment)
	})

	It("should support scaling up or down Azure Spot VM", Label(utils.TestSuiteLabelVMSS, utils.TestSuiteLabelSpotVM), func() {
		By("Checking the spot vms")
		scaleSets, err := utils.ListVMSSes(tc)
		Expect(err).NotTo(HaveOccurred())

		if len(scaleSets) == 0 {
			Skip("At least one VMSS is needed in this scenario")
		}

		foundSpotVMSS := false
		for _, scaleSet := range scaleSets {
			if utils.IsSpotVMSS(scaleSet) {
				foundSpotVMSS = true
				break
			}
		}
		if !foundSpotVMSS {
			Skip("At least one azure spot vm is needed in this scenario")
		}

		utils.Logf("Create a deployment that will trigger a scale up operation")
		replicas := podCount + 1
		deployment := createDeploymentManifest(basename+"-deployment", replicas, map[string]string{"app": basename}, podSize, false)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		waitForScaleUpToComplete(cs, ns, initNodeCount+1)
		waitForScaleDownToComplete(cs, ns, initNodeCount, deployment)
	})

	It("should support scaling up or down due to the consuming of GPU resource", func() {
		utils.Logf("Checking gpu nodes")
		foundGPUNode := false
		gpuCap := int64(0)
		for i := range nodes {
			found, capacity := utils.GetGPUResource(&nodes[i])
			if found {
				gpuCap = capacity
				foundGPUNode = true
				break
			}
		}
		if !foundGPUNode {
			Skip("at least one gpu node is required in this scenario")
		}

		replicas := initNodeCount + 1
		deployment := createDeploymentManifest(basename+"-deployment", int32(replicas), map[string]string{"app": basename}, gpuCap, true)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		waitForScaleUpToComplete(cs, ns, initNodeCount+1)
		waitForScaleDownToComplete(cs, ns, initNodeCount, deployment)
	})
})

func waitForScaleUpToComplete(cs clientset.Interface, ns *v1.Namespace, targetNodeCount int) {
	By("Waiting for a scale up operation to happen")
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

func createPodSpec(requestCPU int64) (result v1.PodSpec) {
	result = v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:  "container",
				Image: "nginx:1.15",
			},
		},
	}
	if requestCPU != 0 {
		result.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse(
					strconv.FormatInt(requestCPU, 10) + "m"),
			},
		}
	}
	return
}

func createGPUPodSpec(requestGPU int64) (result v1.PodSpec) {
	result = v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:  "container",
				Image: "nginx:1.15",
			},
		},
	}
	if requestGPU != 0 {
		result.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				utils.GPUResourceKey: resource.MustParse(
					strconv.FormatInt(requestGPU, 10) + "m"),
			},
			Limits: v1.ResourceList{
				utils.GPUResourceKey: resource.MustParse(
					strconv.FormatInt(requestGPU, 10) + "m"),
			},
		}
	}
	return
}

func createDeploymentManifest(name string, replicas int32, label map[string]string, podSize int64, requestGPU bool) (result *appsv1.Deployment) {
	var spec v1.PodSpec
	if requestGPU {
		spec = createGPUPodSpec(podSize)
	} else {
		spec = createPodSpec(podSize)
	}

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
								v1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}
}

func waitForStatefulSetComplete(cs clientset.Interface, ns *v1.Namespace, ss *appsv1.StatefulSet) error {
	err := wait.PollImmediate(poll, statefulSetTimeout, func() (bool, error) {
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

func calculateNewPodCountOnNode(cs clientset.Interface, node *v1.Node) int32 {
	quantity := node.Status.Allocatable[v1.ResourceCPU]
	runningQuantityOnNode, err := utils.GetNodeRunningQuantity(cs, node.Name)
	Expect(err).NotTo(HaveOccurred())
	emptyQuantity := quantity.DeepCopy()
	emptyQuantity.Sub(runningQuantityOnNode)
	podCountOnNode := int32(math.Floor(float64(emptyQuantity.MilliValue()) / float64(podSize)))
	utils.Logf("found %vm free space on node %s, create %d new pods will saturate the space",
		emptyQuantity.MilliValue(), node.Name, podCountOnNode)

	return podCountOnNode
}

func checkNodeGroupsBalance(nodes *[]v1.Node) bool {
	nodepoolSizeMap := utils.GetNodepoolNodeMap(nodes)
	min, max := math.MaxInt32, math.MinInt32
	for _, nodes := range nodepoolSizeMap {
		size := len(nodes)
		if size < min {
			min = size
		}
		if size > max {
			max = size
		}
	}

	return math.Abs(float64(min-max)) <= 1
}

func isCAEnabled(cs clientset.Interface) (bool, error) {
	deploy, err := cs.AppsV1().Deployments(kubeSystemNamespace).Get(context.TODO(), clusterAutoscalerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	return deploy.Status.ReadyReplicas > 0, nil
}
