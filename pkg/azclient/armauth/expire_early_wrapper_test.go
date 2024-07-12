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

package armauth

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"go.uber.org/mock/gomock"
)

func Test_expireEarlyTokenCredential_GetToken(t *testing.T) {
	type fields struct {
		token azcore.AccessToken
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{
			name: "valid token",
			fields: fields{
				token: azcore.AccessToken{
					Token:     "token",
					ExpiresOn: time.Now().Add(4 * time.Hour),
				},
			},
			wantErr: false,
		},
		{
			name: "valid token",
			fields: fields{
				token: azcore.AccessToken{
					Token:     "token",
					ExpiresOn: time.Now().Add(1 * time.Hour),
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cntl := gomock.NewController(t)
			defer cntl.Finish()
			cred := NewMockTokenCredential(cntl)
			cred.EXPECT().GetToken(gomock.Any(), gomock.Any()).Return(tt.fields.token, nil).AnyTimes()
			w := &expireEarlyTokenCredential{
				cred: cred,
			}
			got, err := w.GetToken(context.Background(), policy.TokenRequestOptions{})
			if (err != nil) != tt.wantErr {
				t.Errorf("expireEarlyTokenCredential.GetToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.ExpiresOn.After(time.Now().Add(2 * time.Hour)) {
				t.Errorf("expireEarlyTokenCredential.GetToken() = %v, should expire within 2 hour", got)
			}
		})
	}
}
