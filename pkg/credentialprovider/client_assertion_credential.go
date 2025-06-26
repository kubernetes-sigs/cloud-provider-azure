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
func NewClientAssertionCredential(tenantID, clientID, authorityHost, assertion string, options *ClientAssertionCredentialOptions) (*ClientAssertionCredential, error) {
	c := &ClientAssertionCredential{assertion: assertion}

	if options == nil {
		options = &ClientAssertionCredentialOptions{}
	}

	cred := confidential.NewCredFromAssertionCallback(
		func(ctx context.Context, _ confidential.AssertionRequestOptions) (string, error) {
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
