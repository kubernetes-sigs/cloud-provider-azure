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

		err = utils.DockerLogin(*registry.Name)
		Expect(err).NotTo(HaveOccurred())

		image := "nginx"
		tag, err := utils.PushImageToACR(*registry.Name, image)
		Expect(tag).NotTo(Equal(""))
		Expect(err).NotTo(HaveOccurred())

		podTemplate := createPodPullingFromACR(*registry.Name, image, tag)
		err = utils.CreatePod(cs, ns.Name, podTemplate)
		Expect(err).NotTo(HaveOccurred())

		result, err := utils.WaitPodTo(v1.PodRunning, cs, podTemplate, ns.Name)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())

		err = utils.DockerLogout()
		Expect(err).NotTo(HaveOccurred())
	})
})

func createPodPullingFromACR(registryName, image, tag string) (result *v1.Pod) {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-test", image),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test-app",
					Image: registryName + ".azurecr.io/" + image + ":" + tag,
				},
			},
		},
	}
}
