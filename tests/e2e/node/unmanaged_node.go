/*
Copyright 2023 The Kubernetes Authors.

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
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Unmanaged nodes", Label(utils.TestSuiteUnmanagedNode), func() {
	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cs = nil
	})

	It("unmanaged Nodes should not be removed", func() {
		unmanagedLabel := map[string]string{consts.ManagedByAzureLabel: "false"}
		nodeName := "fake-node"
		utils.Logf("Creating a fake Node %q", nodeName)
		node := utils.CreateNodeManifest(nodeName, unmanagedLabel)
		_, err := cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		defer func() {
			err := utils.DeleteNodes(cs, []string{nodeName})
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Expect the Node %q not to be removed", nodeName)
		err = wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				if apierrs.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			return false, nil
		})
		Expect(err).To(HaveOccurred(), "Node should not be removed")
		// Error may be:
		// * client rate limiter Wait returned an error: context deadline exceeded
		// * client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline
		Expect(wait.Interrupted(err) || strings.Contains(err.Error(), "exceed context deadline")).To(BeTrue(), fmt.Sprintf("Error should be Interrupted, actually: %v", err))
	})
})
