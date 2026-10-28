/*
Copyright 2026 The Kubernetes Authors.

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
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	servicehelper "k8s.io/cloud-provider/service/helpers"
)

func TestWaitServiceExposureAndValidateConnectivity(t *testing.T) {
	connectivityErr := errors.New("connectivity timeout")
	exposureErr := errors.New("exposure failed")
	podErr := errors.New("pod creation failed")
	cleanupErr := apierrs.NewForbidden(schema.GroupResource{Resource: "services"}, "test-service", errors.New("cleanup denied"))
	testcases := []struct {
		name              string
		failValidationAt  int
		exposureErr       error
		podErr            error
		serviceCleanupErr error
		podCleanupErr     error
		wantErr           error
		wantChecks        int
	}{
		{name: "initial connectivity failure", failValidationAt: 1, wantErr: connectivityErr, wantChecks: 1},
		{name: "IPv6 connectivity failure", failValidationAt: 2, wantErr: connectivityErr, wantChecks: 2},
		{name: "successful validation", wantChecks: 2},
		{name: "exposure failure", exposureErr: exposureErr, wantErr: exposureErr},
		{name: "pod creation failure", podErr: podErr, wantErr: podErr},
		{name: "service cleanup failure preserves connectivity error", failValidationAt: 1, serviceCleanupErr: cleanupErr, wantErr: connectivityErr, wantChecks: 1},
		{name: "pod cleanup failure preserves connectivity error", failValidationAt: 1, podCleanupErr: cleanupErr, wantErr: connectivityErr, wantChecks: 1},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			var logs bytes.Buffer
			ginkgo.GinkgoWriter.TeeTo(&logs)
			t.Cleanup(ginkgo.GinkgoWriter.ClearTeeWriters)
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test-service", Namespace: "test-ns"},
				Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
				Status: v1.ServiceStatus{LoadBalancer: v1.LoadBalancerStatus{
					Ingress: []v1.LoadBalancerIngress{{IP: "192.0.2.1"}, {IP: "2001:db8::1"}},
				}},
			}
			cs := fake.NewClientset(service)
			if testcase.exposureErr != nil {
				exposureFailed := false
				cs.PrependReactor("get", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
					if exposureFailed {
						return false, nil, nil
					}
					exposureFailed = true
					return true, nil, testcase.exposureErr
				})
			}
			cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if testcase.podErr != nil {
					return true, nil, testcase.podErr
				}
				pod := action.(clienttesting.CreateAction).GetObject().(*v1.Pod)
				pod.Status.Phase = v1.PodRunning
				return false, nil, nil
			})
			serviceDeletes := 0
			cs.PrependReactor("delete", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
				serviceDeletes++
				return testcase.serviceCleanupErr != nil, nil, testcase.serviceCleanupErr
			})
			cs.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
				return testcase.podCleanupErr != nil, nil, testcase.podCleanupErr
			})
			checks := 0
			ips, err := waitServiceExposureAndValidateConnectivity(cs, service.Namespace, service.Name, nil,
				func(namespace, pod, ip string, port int, protocol v1.Protocol) error {
					assert.Equal(t, service.Namespace, namespace)
					assert.Equal(t, ExecAgnhostPod, pod)
					assert.Equal(t, service.Status.LoadBalancer.Ingress[checks].IP, ip)
					assert.Equal(t, 80, port)
					assert.Equal(t, v1.ProtocolTCP, protocol)
					checks++
					if checks == testcase.failValidationAt {
						return connectivityErr
					}
					return nil
				})
			if testcase.wantErr == nil {
				require.NoError(t, err)
				assert.Zero(t, serviceDeletes)
			} else {
				require.ErrorIs(t, err, testcase.wantErr)
				if testcase.wantErr == connectivityErr {
					assert.Same(t, connectivityErr, err)
				}
				assert.Equal(t, 1, serviceDeletes)
			}
			assert.Equal(t, testcase.wantChecks, checks)
			if testcase.exposureErr == nil {
				assert.Equal(t, []string{"192.0.2.1", "2001:db8::1"}, StrPtrSliceToStrSlice(ips))
			}
			_, getErr := cs.Tracker().Get(v1.SchemeGroupVersion.WithResource("services"), service.Namespace, service.Name)
			if testcase.wantErr == nil || testcase.serviceCleanupErr != nil {
				assert.NoError(t, getErr)
			} else {
				assert.True(t, apierrs.IsNotFound(getErr), "service should have been deleted: %v", getErr)
			}
			if testcase.serviceCleanupErr != nil {
				assert.Contains(t, logs.String(), "Failed to clean up Service")
				assert.Contains(t, logs.String(), testcase.serviceCleanupErr.Error())
				assert.ErrorIs(t, DeleteService(cs, service.Namespace, service.Name), testcase.serviceCleanupErr)
			} else {
				require.NoError(t, DeleteService(cs, service.Namespace, service.Name))
				require.NoError(t, DeleteService(cs, service.Namespace, service.Name))
			}
			if testcase.podCleanupErr != nil {
				assert.Contains(t, logs.String(), "failed to delete ExecAgnhostPod, error: "+testcase.podCleanupErr.Error())
			} else {
				_, getErr = cs.CoreV1().Pods(service.Namespace).Get(context.Background(), ExecAgnhostPod, metav1.GetOptions{})
				assert.True(t, apierrs.IsNotFound(getErr), "exec pod should have been deleted: %v", getErr)
			}
		})
	}
}

func TestDeleteServiceWaitsForFinalization(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "test-service", Namespace: "test-ns",
		Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
	}}
	cs := fake.NewClientset(service)
	cs.PrependReactor("delete", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		now := metav1.Now()
		service.DeletionTimestamp = &now
		return true, nil, nil
	})
	gets := 0
	cs.PrependReactor("get", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, service, nil
		}
		return true, nil, apierrs.NewNotFound(schema.GroupResource{Resource: "services"}, service.Name)
	})
	require.NoError(t, DeleteService(cs, service.Namespace, service.Name))
	assert.Equal(t, 2, gets)
}
