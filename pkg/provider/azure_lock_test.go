/*
Copyright 2024 The Kubernetes Authors.

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

package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	client_go_testing "k8s.io/client-go/testing"
)

func newTestAzureResourceLocker(ctrl *gomock.Controller, kubeClient kubernetes.Interface) *AzureResourceLocker {
	cloud := GetTestCloud(ctrl)
	cloud.KubeClient = kubeClient
	return NewAzureResourceLocker(cloud, "holder", "aks-managed-resource-locker", "kube-system", 900)
}

func TestLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc        string
		getErr      bool
		createErr   bool
		updateErr   bool
		existed     bool
		expired     bool
		sameHolder  bool
		expectedErr string
	}{
		{
			desc:        "should return an error if failed to get the lease",
			getErr:      true,
			expectedErr: "failed to get leases",
		},
		{
			desc:        "should return an error if failed to create the lease",
			createErr:   true,
			expectedErr: "failed to create lease",
		},
		{
			desc:        "should return an error if failed to get the lease when acquiring it",
			getErr:      true,
			existed:     true,
			expectedErr: "failed to get leases",
		},
		{
			desc:        "should return an error when trying to acquire a lease which has not expired",
			existed:     true,
			expectedErr: "Lease has not expired yet",
		},
		{
			desc:       "should not return an error if the lease has not expired but the previous holder is the same",
			existed:    true,
			sameHolder: true,
		},
		{
			desc:        "should return an error if failed to update the lease",
			existed:     true,
			updateErr:   true,
			expired:     true,
			expectedErr: "failed to update lease",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			testLease := getTestLease()
			kubeClient := fake.NewSimpleClientset()
			if !tc.expired {
				testLease.Spec.HolderIdentity = to.Ptr("prevHolder")
				testLease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-100)}
				if tc.sameHolder {
					testLease.Spec.HolderIdentity = to.Ptr("holder")
				}
			}
			if tc.existed {
				kubeClient = fake.NewSimpleClientset(testLease)
			}
			getCount := 0
			if tc.getErr {
				kubeClient.PrependReactor(
					"get", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						getCount++
						if tc.existed {
							if getCount == 2 {
								return true, nil, errors.New("failed to get leases")
							}
							return true, testLease, nil
						}
						return true, nil, errors.New("failed to get leases")
					},
				)
			}
			if tc.createErr {
				kubeClient.PrependReactor(
					"create", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("failed to create lease")
					},
				)
			}
			if tc.updateErr {
				kubeClient.PrependReactor(
					"update", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("failed to update lease")
					},
				)
			}
			if !tc.getErr && !tc.createErr {
				kubeClient.PrependReactor(
					"get", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						return true, testLease, nil
					},
				)
			}

			locker := newTestAzureResourceLocker(ctrl, kubeClient)
			err := locker.Lock(context.TODO())
			if tc.expectedErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

func TestUnlock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc            string
		getErr          bool
		updateErr       bool
		expired         bool
		differentHolder bool
		expectedErr     string
	}{
		{
			desc:        "should return an error if failed to get the lease",
			getErr:      true,
			expectedErr: "failed to get lease",
		},
		{
			desc:    "should do nothing if the lease has expired",
			expired: true,
		},
		{
			desc:            "should do nothing if the lease is holding by another holder",
			differentHolder: true,
		},
		{
			desc:        "should return an error if failed to update the lease",
			updateErr:   true,
			expired:     true,
			expectedErr: "failed to update lease",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			testLease := getTestLease()
			testLease.Spec.HolderIdentity = to.Ptr("holder")
			testLease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-1000)}
			if !tc.expired {
				testLease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-100)}
				if tc.differentHolder {
					testLease.Spec.HolderIdentity = to.Ptr("prevHolder")
				}
			}

			kubeClient := fake.NewSimpleClientset(testLease)
			if tc.getErr {
				kubeClient.PrependReactor(
					"get", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("failed to get lease")
					},
				)
			}
			if tc.updateErr {
				kubeClient.PrependReactor(
					"update", "leases",
					func(_ client_go_testing.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("failed to update lease")
					},
				)
			}

			locker := newTestAzureResourceLocker(ctrl, kubeClient)
			err := locker.Unlock(context.TODO())
			if tc.expectedErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

func getTestLease() *v1.Lease {
	return &v1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aks-managed-resource-locker",
			Namespace: "kube-system",
		},
		Spec: v1.LeaseSpec{
			HolderIdentity:       to.Ptr(""),
			LeaseDurationSeconds: to.Ptr[int32](900),
		},
	}
}
