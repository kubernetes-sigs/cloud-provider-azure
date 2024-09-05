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

package armbalancer

import (
	"context"
	"net/http"
	"testing"

	"go.uber.org/mock/gomock"
	"golang.org/x/sync/errgroup"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils/armbalancer/mock"
)

func Test_transportChannPool_Run(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	pool := newtransportChannPool(10, func() Transport {
		MockTransport := mock.NewMockTransport(ctrl)
		MockTransport.EXPECT().ForceClose().Return(nil).AnyTimes()
		MockTransport.EXPECT().RoundTrip(gomock.Any()).Return(nil, nil).AnyTimes()
		return MockTransport
	})
	ctx, cancel := context.WithCancel(context.Background())
	var serverGrp errgroup.Group
	serverGrp.SetLimit(-1)
	serverGrp.Go(func() error {
		return pool.Run(ctx)
	})

	req, err := http.NewRequest(http.MethodGet, "http://www.example.com", nil)
	if err != nil {
		t.Fatal("http.NewRequest should not return error")
	}
	resp, err := pool.RoundTrip(req)
	if err != nil {
		t.Fatalf("http.RoundTrip should not return error, got: %+v", err)
	}
	if resp != nil {
		t.Fatal("http.RoundTrip should return nil response")
	}
	cancel()
	if err := serverGrp.Wait(); err != nil {
		t.Fatal(err)
	}
}

func Benchmark_testRoundtripperPool(b *testing.B) {
	ctrl := gomock.NewController(b)
	defer ctrl.Finish()
	pool := newtransportChannPool(10, func() Transport {
		MockTransport := mock.NewMockTransport(ctrl)
		MockTransport.EXPECT().ForceClose().Return(nil).AnyTimes()
		MockTransport.EXPECT().RoundTrip(gomock.Any()).DoAndReturn(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{}, nil
		}).AnyTimes()
		return MockTransport
	})
	ctx, cancelFn := context.WithCancel(context.Background())
	var serverGrp errgroup.Group
	serverGrp.Go(func() error {
		return pool.Run(ctx)
	})
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("GET", "http://example.com", nil)
			_, err := pool.RoundTrip(req)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.StopTimer()
	cancelFn()

	if err := serverGrp.Wait(); err != nil {
		b.Fatal(err)
	}
}
