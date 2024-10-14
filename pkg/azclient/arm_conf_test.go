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

package azclient

import (
	"reflect"
	"testing"
)

func TestUserAgent(t *testing.T) {
	type args struct {
		armConfig *ARMClientConfig
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name: "user agent",
			args: args{
				armConfig: &ARMClientConfig{
					UserAgent: "test",
				},
			},
			want:    "test",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetAzCoreClientOption(tt.args.armConfig)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetAzCoreClientOption() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got.Telemetry.ApplicationID, tt.want) {
				t.Errorf("GetAzCoreClientOption() = %v, want %v", got, tt.want)
			}
		})
	}
}
