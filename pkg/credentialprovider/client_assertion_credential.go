/*
Copyright 2025 The Kubernetes Authors.

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

// Source: https://github.com/Azure/azure-workload-identity/blob/69b764195fca4581bec100d5d12319c9205b76fa/examples/msal-go/token_credential.go

package credentialprovider

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"
)

// ClientAssertionCredential authenticates an application with assertions provided by a callback function.
type ClientAssertionCredential struct {
	assertion string
	client    confidential.Client
}

// ClientAssertionCredentialOptions contains optional parameters for ClientAssertionCredential.
type ClientAssertionCredentialOptions struct {
	azcore.ClientOptions
}

// NewClientAssertionCredential constructs a clientAssertionCredential. Pass nil for options to accept defaults.
func NewClientAssertionCredential(tenantID, clientID, authorityHost, assertion string) (*ClientAssertionCredential, error) {
	c := &ClientAssertionCredential{assertion: assertion}

	cred := confidential.NewCredFromAssertionCallback(
		func(_ context.Context, _ confidential.AssertionRequestOptions) (string, error) {
			return c.assertion, nil
		},
	)

	authority, err := url.JoinPath(authorityHost, tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to construct authority URL: %w", err)
	}

	client, err := confidential.New(authority, clientID, cred)
	if err != nil {
		return nil, fmt.Errorf("failed to create confidential client: %w", err)
	}
	c.client = client

	return c, nil
}

// GetToken requests an AAD access token for the specified set of scopes.
func (c *ClientAssertionCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	// get the token from the confidential client
	token, err := c.client.AcquireTokenByCredential(ctx, opts.Scopes)
	if err != nil {
		return azcore.AccessToken{}, err
	}

	return azcore.AccessToken{
		Token:     token.AccessToken,
		ExpiresOn: token.ExpiresOn,
	}, nil
}
