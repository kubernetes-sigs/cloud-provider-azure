/*
Copyright 2019 The Kubernetes Authors.

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

package auth

import (
	"fmt"
	"strings"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Azure Credential Provider", Label(utils.TestSuiteLabelCredential), func() {
	var err error
	var tc *utils.AzureTestClient
	var cs clientset.Interface
	var ns *v1.Namespace

	BeforeEach(func() {
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace("cred", cs)
		Expect(err).NotTo(HaveOccurred())

		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if ns != nil && cs != nil {
			err = utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
		}

		cs = nil
		ns = nil
		tc = nil
	})

	It("should be able to pull private images from acr without docker secrets set explicitly", func() {
		registry, err := tc.CreateContainerRegistry()
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			err = tc.DeleteContainerRegistry(*registry.Name)
			if err != nil {
				utils.Logf("failed to cleanup registry with error: %w", err)
			}
		}()

		image := "nginx"
		tag, err := tc.PushImageToACR(*registry.Name, image)
		Expect(err).NotTo(HaveOccurred())
		Expect(tag).NotTo(Equal(""))

		acrImageURL := fmt.Sprintf("%s.azurecr.io/%s:%s", *registry.Name, image, tag)
		podTemplate := createPodPullingFromACR(image, acrImageURL, "linux")
		err = utils.CreatePod(cs, ns.Name, podTemplate)
		Expect(err).NotTo(HaveOccurred())

		result, err := utils.WaitPodTo(v1.PodRunning, cs, podTemplate, ns.Name)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
	})

	It("should be able to create an ACR cache and pull images from it", Label(utils.TestSuiteLabelOOTCredential), func() {
		// This test involves a preview feature ACR: ACR cache
		// https://learn.microsoft.com/en-us/azure/container-registry/tutorial-registry-cache
		// The reason is that Windows test should be included but control-plane Node is Linux.
		// So, it is hard to push a Windows image to ACR. With this new feature, the pushing can be skipped.
		registry, err := tc.CreateContainerRegistry()
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			err = tc.DeleteContainerRegistry(*registry.Name)
			if err != nil {
				utils.Logf("failed to cleanup registry with error: %w", err)
			}
		}()

		// az acr login
		Expect(registry.Name).NotTo(BeNil())
		err = utils.AZACRLogin(*registry.Name)
		Expect(err).NotTo(HaveOccurred())

		testPull := func(imageURL, imageTag, os string) {
			imageNameSlice := strings.Split(imageURL, "/")
			imageName := imageNameSlice[len(imageNameSlice)-1]
			acrImageURL := fmt.Sprintf("%s.azurecr.io/%s:%s", *registry.Name, imageName, imageTag)

			err = utils.AZACRCacheCreate(*registry.Name, fmt.Sprintf("%s-cache-rule", imageName), imageURL, imageName)
			Expect(err).NotTo(HaveOccurred())

			podTemplate := createPodPullingFromACR(imageName, acrImageURL, os)
			err = utils.CreatePod(cs, ns.Name, podTemplate)
			Expect(err).NotTo(HaveOccurred())

			result, err := utils.WaitPodTo(v1.PodRunning, cs, podTemplate, ns.Name)
			Expect(result).To(BeTrue())
			Expect(err).NotTo(HaveOccurred())
		}

		testPull("mcr.microsoft.com/mirror/docker/library/nginx", "1.25", "linux")
		if tc.HasWindowsNodes {
			testPull("mcr.microsoft.com/windows/nanoserver", "ltsc2019", "windows")
		}
	})
})

func createPodPullingFromACR(imageName, acrImageURL string, os string) (result *v1.Pod) {
	utils.Logf("Creating a Pod with ACR image URL %q", acrImageURL)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-test-%s", imageName, os),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test-app",
					Image: acrImageURL,
				},
			},
			NodeSelector: map[string]string{
				v1.LabelOSStable: os,
			},
			Tolerations: []v1.Toleration{
				{
					Key:      consts.ControlPlaneNodeRoleLabel,
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
				{
					Key:      consts.MasterNodeRoleLabel,
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
			},
		},
	}
}
