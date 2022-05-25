/*
Copyright 2022 The Kubernetes Authors.

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

package node

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Lifecycle of VMSS", Label(utils.TestSuiteLabelVMSS), func() {
	var (
		ns     *v1.Namespace
		k8sCli kubernetes.Interface
		azCli  *utils.AzureTestClient
	)

	BeforeEach(func() {
		const Basename = "vmss-lifecycle"
		var err error
		k8sCli, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(Basename, k8sCli)
		Expect(err).NotTo(HaveOccurred())

		azCli, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := utils.DeleteNamespace(k8sCli, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		k8sCli = nil
		ns = nil
		azCli = nil
	})

	It("should delete node object when VMSS instance deallocated", func() {
		By("fetch VMSS")
		vmss, err := utils.FindTestVMSS(azCli, azCli.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())
		if vmss == nil {
			Skip("skip non-VMSS")
		}
		numInstance := *vmss.Sku.Capacity
		utils.Logf("Current VMSS %q sku capacity: %d", *vmss.Name, numInstance)
		expectedCap := map[string]int64{*vmss.Name: numInstance}
		originalNodes, err := utils.GetAgentNodes(k8sCli)
		Expect(err).NotTo(HaveOccurred())

		By("deallocate VMSS instance")
		if strings.EqualFold(os.Getenv(utils.CAPZTestCCM), "true") {
			err = utils.ScaleMachinePool(*vmss.Name, numInstance-1)
		} else {
			err = utils.ScaleVMSS(azCli, *vmss.Name, azCli.GetResourceGroup(), numInstance-1)
		}
		Expect(err).NotTo(HaveOccurred())
		expectedCap[*vmss.Name] = numInstance - 1

		defer func() {
			By("reset VMSS instance")
			if strings.EqualFold(os.Getenv(utils.CAPZTestCCM), "true") {
				err = utils.ScaleMachinePool(*vmss.Name, numInstance)
			} else {
				err = utils.ScaleVMSS(azCli, *vmss.Name, azCli.GetResourceGroup(), numInstance)
			}
			Expect(err).NotTo(HaveOccurred())
			expectedCap[*vmss.Name] = numInstance

			err = utils.ValidateClusterNodesMatchVMSSInstances(azCli, expectedCap, originalNodes)
			Expect(err).NotTo(HaveOccurred())

			vmssAfterTest, err := utils.GetVMSS(azCli, *vmss.Name)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("VMSS %q sku capacity after the test: %d", *vmssAfterTest.Name, *vmssAfterTest.Sku.Capacity)
		}()

		err = utils.ValidateClusterNodesMatchVMSSInstances(azCli, expectedCap, originalNodes)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should add node object when VMSS instance allocated", func() {
		By("fetch VMSS")
		vmss, err := utils.FindTestVMSS(azCli, azCli.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())
		if vmss == nil {
			Skip("skip non-VMSS")
		}
		numInstance := *vmss.Sku.Capacity
		utils.Logf("Current VMSS %q sku capacity: %d", *vmss.Name, numInstance)
		expectedCap := map[string]int64{*vmss.Name: numInstance}
		originalNodes, err := utils.GetAgentNodes(k8sCli)
		Expect(err).NotTo(HaveOccurred())

		By("allocate VMSS instance")
		if strings.EqualFold(os.Getenv(utils.CAPZTestCCM), "true") {
			err = utils.ScaleMachinePool(*vmss.Name, numInstance+1)
		} else {
			err = utils.ScaleVMSS(azCli, *vmss.Name, azCli.GetResourceGroup(), numInstance+1)
		}
		Expect(err).NotTo(HaveOccurred())
		expectedCap[*vmss.Name] = numInstance + 1

		defer func() {
			By("reset VMSS instance")
			if strings.EqualFold(os.Getenv(utils.CAPZTestCCM), "true") {
				err = utils.ScaleMachinePool(*vmss.Name, numInstance)
			} else {
				err = utils.ScaleVMSS(azCli, *vmss.Name, azCli.GetResourceGroup(), numInstance)
			}
			Expect(err).NotTo(HaveOccurred())
			expectedCap[*vmss.Name] = numInstance

			err = utils.ValidateClusterNodesMatchVMSSInstances(azCli, expectedCap, originalNodes)
			Expect(err).NotTo(HaveOccurred())

			vmssAfterTest, err := utils.GetVMSS(azCli, *vmss.Name)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("VMSS %q sku capacity after the test: %d", *vmssAfterTest.Name, *vmssAfterTest.Sku.Capacity)
		}()

		err = utils.ValidateClusterNodesMatchVMSSInstances(azCli, expectedCap, originalNodes)
		Expect(err).NotTo(HaveOccurred())
	})
})
