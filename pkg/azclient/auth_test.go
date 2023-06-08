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

package azclient_test

import (
	"context"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	azpolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var _ = Describe("Auth", Ordered, func() {
	var tenantID string = "tenantid"
	var clientID string = "clientid"

	var cred azcore.TokenCredential
	var authProvider *azclient.AuthProvider
	var httpRecorder *recorder.Recorder
	var clientOptions *policy.ClientOptions
	var err error
	BeforeAll(func() {
		httpRecorder, err = recorder.NewWithOptions(&recorder.Options{
			CassetteName:       "testdata/auth",
			Mode:               recorder.ModeRecordOnce,
			RealTransport:      utils.DefaultTransport,
			SkipRequestLatency: true,
		})
		httpRecorder.SetReplayableInteractions(true)

		Expect(err).NotTo(HaveOccurred())
		if httpRecorder.IsNewCassette() {
			tenantID = os.Getenv(azclient.AzureTenantID)
			clientID = os.Getenv(azclient.AzureClientID)
		}
		httpRecorder.AddHook(func(i *cassette.Interaction) error {
			i.Request.URL = strings.Replace(i.Request.URL, tenantID, "tenantid", -1)
			i.Request.Body = strings.Replace(i.Request.Body, tenantID, "tenantid", -1)
			i.Response.Body = strings.Replace(i.Response.Body, tenantID, "tenantid", -1)

			i.Request.URL = strings.Replace(i.Request.URL, clientID, "clientid", -1)
			i.Request.Body = strings.Replace(i.Request.Body, clientID, "clientid", -1)
			i.Response.Body = strings.Replace(i.Response.Body, clientID, "clientid", -1)
			if i.Request.Form.Has("client_id") {
				i.Request.Form.Set("client_id", clientID)
			}

			delete(i.Response.Headers, "Set-Cookie")
			delete(i.Response.Headers, "Date")
			delete(i.Response.Headers, "X-Ms-Request-Id")
			delete(i.Response.Headers, "X-Ms-Ests-Server")
			delete(i.Response.Headers, "Content-Security-Policy-Report-Only")

			if strings.Contains(i.Response.Body, "access_token") {
				i.Response.Body = `{"token_type":"Bearer","expires_in":86399,"ext_expires_in":86399,"access_token":"faketoken"}`
			}

			return nil
		}, recorder.BeforeSaveHook)
		clientOptions, err = azclient.GetDefaultAuthClientOption(nil)
		Expect(err).NotTo(HaveOccurred())

		// armloadbalancer doesn't support ligin.microsoft.com
		clientOptions.Transport = httpRecorder.GetDefaultClient()
	})

	When("AADClientSecret is set", func() {
		It("should return a valid token", func() {
			clientSecret := "clientSecret"
			if httpRecorder.IsNewCassette() {
				clientSecret = os.Getenv("AZURE_CLIENT_SECRET")
			}
			httpRecorder.AddHook(func(i *cassette.Interaction) error {
				i.Request.URL = strings.Replace(i.Request.URL, clientSecret, "clientsecret", -1)
				i.Request.Body = strings.Replace(i.Request.Body, clientSecret, "clientsecret", -1)
				i.Response.Body = strings.Replace(i.Response.Body, clientSecret, "clientsecret", -1)
				if i.Request.Form.Has("client_secret") {
					i.Request.Form.Set("client_secret", "clientsecret")
				}
				return nil
			}, recorder.BeforeSaveHook)
			Expect(err).NotTo(HaveOccurred())
			authProvider, err = azclient.NewAuthProvider(azclient.AzureAuthConfig{
				TenantID:        tenantID,
				AADClientID:     clientID,
				AADClientSecret: clientSecret,
			}, clientOptions)
			Expect(err).NotTo(HaveOccurred())
			cred, err = authProvider.GetAzIdentity()
			Expect(err).NotTo(HaveOccurred())
			Expect(cred).NotTo(BeNil())
			token, err := cred.GetToken(context.Background(), azpolicy.TokenRequestOptions{
				Scopes:   []string{"https://management.azure.com/.default"},
				TenantID: tenantID,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(token.Token).NotTo(BeEmpty())
		})
	})
	AfterAll(func() {
		httpRecorder.Stop()
	})
})
