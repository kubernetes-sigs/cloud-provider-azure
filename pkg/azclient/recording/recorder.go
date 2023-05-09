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

package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/google/uuid"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	gorecorder "gopkg.in/dnaeon/go-vcr.v3/recorder"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var (
	dateMatcher   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(.\d+)?Z`)
	sshKeyMatcher = regexp.MustCompile("ssh-rsa [0-9a-zA-Z+/=]+")

	// This is pretty involved, here's the breakdown of what each bit means:
	// [p|P]assword":\s*" - find any JSON field that ends in the string password, followed by any number of spaces and another quote.
	// ((?:[^\\"]*?(?:(\\\\)|(\\"))*?)*?) - The outer group is a capturing group, which selects the actual password
	// [^\\"]*? - this matches any characters that aren't \ or " (need to handle them specially because of escaped quotes)
	// (?:(?:\\\\)|(?:\\")|(?:\\))*? - lazily match any number of escaped backslahes or escaped quotes.
	// The above two sections are repeated until the first unescaped "s
	passwordMatcher = regexp.MustCompile(`[p|P]assword":\s*"((?:[^\\"]*?(?:(?:\\\\)|(?:\\")|(?:\\))*?)*?)"`)

	// keyMatcher matches any valid base64 value with at least 10 sets of 4 bytes of data that ends in = or ==.
	// Both storage account keys and Redis account keys are longer than that and end in = or ==. Note that technically
	// base64 values need not end in == or =, but allowing for that in the match will flag tons of false positives as
	// any text (including long URLs) have strings of characters that meet this requirement. There are other base64 values
	// in the payloads (such as operationResults URLs for polling async operations for some services) that seem to use
	// very long base64 strings as well.
	keyMatcher = regexp.MustCompile("(?:[A-Za-z0-9+/]{4}){10,}(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)")
)

// hideDates replaces all ISO8601 datetimes with a fixed value
// this lets us match requests that may contain time-sensitive information (timestamps, etc)
func hideDates(s string) string {
	return dateMatcher.ReplaceAllLiteralString(s, "2001-02-03T04:05:06Z") // this should be recognizable/parseable as a fake date
}

// hideSSHKeys hides anything that looks like SSH keys
func hideSSHKeys(s string) string {
	return sshKeyMatcher.ReplaceAllLiteralString(s, "ssh-rsa {KEY}")
}

// hidePasswords hides anything that looks like a generated password
func hidePasswords(s string) string {
	matches := passwordMatcher.FindAllStringSubmatch(s, -1)
	for _, match := range matches {
		for n, submatch := range match {
			if n%2 == 0 {
				continue
			}
			s = strings.ReplaceAll(s, submatch, "{PASSWORD}")
		}
	}
	return s
}

func hideKeys(s string) string {
	return keyMatcher.ReplaceAllLiteralString(s, "{KEY}")
}

func hideRecordingData(s string) string {
	result := hideDates(s)
	result = hideSSHKeys(result)
	result = hidePasswords(result)
	result = hideKeys(result)

	return result
}

type Recorder struct {
	cassetteName   string
	credential     azcore.TokenCredential
	rec            *gorecorder.Recorder
	subscriptionID string

	httpClient *http.Client
}

type DummyTokenCredential func(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error)

func (d DummyTokenCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return d(ctx, opts)
}

func NewRecorder(cassetteName string) (*Recorder, error) {
	rec, err := gorecorder.NewWithOptions(&gorecorder.Options{
		CassetteName:       cassetteName,
		Mode:               gorecorder.ModeRecordOnce,
		SkipRequestLatency: true,
		RealTransport:      http.DefaultTransport,
	})
	if err != nil {
		return nil, err
	}

	rec.SetRealTransport(utils.DefaultTransport)
	rec.SetReplayableInteractions(true)
	var tokenCredential azcore.TokenCredential
	var subscriptionID string

	if rec.IsRecording() {
		subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
		if subscriptionID == "" {
			return nil, errors.New("required environment variable AZURE_SUBSCRIPTION_ID was not supplied")
		}
		tokenCredential, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, err
		}
	} else {
		// if we are replaying, we won't need auth
		// and we use a dummy subscription ID
		subscriptionID = uuid.Nil.String()
		tokenCredential = DummyTokenCredential(func(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
			return azcore.AccessToken{}, nil
		})
	}

	// check body as well as URL/Method (copied from go-vcr documentation)
	rec.SetMatcher(func(r *http.Request, i cassette.Request) bool {
		if !cassette.DefaultMatcher(r, i) {
			return false
		}

		// verify custom request count header (see counting_roundtripper.go)
		if r.Header.Get(countHeader) != i.Headers.Get(countHeader) {
			return false
		}

		if r.Body == nil {
			return i.Body == ""
		}

		var b bytes.Buffer
		if _, err := b.ReadFrom(r.Body); err != nil {
			panic(err)
		}

		r.Body = io.NopCloser(&b)
		return b.String() == "" || hideRecordingData(b.String()) == i.Body
	})

	hook := func(i *cassette.Interaction) error {
		// rewrite all request/response fields to hide the real subscription ID
		// this is *not* a security measure but intended to make the tests updateable from
		// any subscription, so a contributor can update the tests against their own sub.
		hideSubID := func(s string) string {
			return strings.ReplaceAll(s, subscriptionID, uuid.Nil.String())
		}

		i.Request.Body = hideRecordingData(hideSubID(i.Request.Body))
		i.Response.Body = hideRecordingData(hideSubID(i.Response.Body))
		i.Request.URL = hideSubID(i.Request.URL)

		for _, values := range i.Request.Headers {
			for i := range values {
				values[i] = hideSubID(values[i])
			}
		}

		for _, values := range i.Response.Headers {
			for i := range values {
				values[i] = hideSubID(values[i])
			}
		}

		for _, header := range requestHeadersToRemove {
			delete(i.Request.Headers, header)
		}

		for _, header := range responseHeadersToRemove {
			delete(i.Response.Headers, header)
		}

		return nil
	}
	rec.AddHook(hook, gorecorder.BeforeSaveHook)

	return &Recorder{
		cassetteName:   cassetteName,
		credential:     tokenCredential,
		rec:            rec,
		subscriptionID: subscriptionID,
	}, nil
}

func (r *Recorder) HTTPClient() *http.Client {
	if r.httpClient == nil {
		r.httpClient = &http.Client{
			Transport: addCountHeader(translateErrors(r.rec, r.cassetteName)),
		}
	}

	return r.httpClient
}

func (r *Recorder) TokenCredential() azcore.TokenCredential {
	return r.credential
}

func (r *Recorder) SubscriptionID() string {
	return r.subscriptionID
}

func (r *Recorder) Stop() error {
	return r.rec.Stop()
}
