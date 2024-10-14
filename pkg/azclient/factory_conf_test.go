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

func TestGetDefaultResourceClientOptionUserAgent(t *testing.T) {
	type args struct {
		armConfig     *ARMClientConfig
		factoryConfig *ClientFactoryConfig
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
			got, err := GetDefaultResourceClientOption(tt.args.armConfig, tt.args.factoryConfig)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetDefaultResourceClientOption() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got.Telemetry.ApplicationID, tt.want) {
				t.Errorf("GetDefaultResourceClientOption() = %v, want %v", got, tt.want)
			}
			// cred, _ := azidentity.NewDefaultAzureCredential(nil)
			// clientFactory, err := NewClientFactory(nil, tt.args.armConfig, cred)
			// if err != nil {
			// 	t.Error("NewClientFactory() should retry non empty value")
			// }
			// diskClient := clientFactory.GetDiskClient()
			// if diskClient == nil {
			// 	t.Error("GetDiskClient() should retry non empty value")
			// }
			// impl := diskClient.(*diskclient.Client)
			// if impl.DisksClient.internal. != tt.want {
			// 	t.Errorf("GetDefaultResourceClientOption() = %v, want %v", impl.DisksClient.Telemetry.ApplicationID, tt.want)
			// }
		})
	}
}
