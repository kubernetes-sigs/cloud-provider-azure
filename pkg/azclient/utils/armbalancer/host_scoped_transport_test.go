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

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils/armbalancer/mock"
)

func Benchmark_testhostScopedTransport(b *testing.B) {
	ctrl := gomock.NewController(b)
	defer ctrl.Finish()
	ctx := context.Background()
	transport := NewHostScopedTransport(ctx, func() *transportChannPool {
		return newtransportChannPool(10, func() Transport {
			MockTransport := mock.NewMockTransport(ctrl)
			MockTransport.EXPECT().ForceClose().Return(nil).AnyTimes()
			MockTransport.EXPECT().RoundTrip(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
				return &http.Response{}, nil
			}).AnyTimes()
			return MockTransport
		})
	})
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		flag := 1
		counter := 1
		for pb.Next() {
			var host string
			if counter^flag == 0 {
				host = "http://www.example.com"
			} else {
				host = "http://www.example.org"
			}
			req, _ := http.NewRequest("GET", host, nil)
			_, err := transport.RoundTrip(req)
			if err != nil {
				b.Fatal(err)
			}
			counter = counter ^ flag
		}
	})

	b.StopTimer()
	transport.ForceClose()
}
