/*
Copyright 2022 The Kubernetes Authors.

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

package retry

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	// TODO(mainred): etag mismatch error message is not consistent across Azure APIs, we need to normalize it or add more error messages.
	// Example error message for CRP
	// Etag provided in if-match header {0} does not match etag {1} of resource.
	//   where {0} and {1} are the etags provided in the request and the resource respectively.
	EtagMismatchPattern = `Etag provided in if-match header ([^\s]+) does not match etag ([^\s]+) of resource`

	EtagMismatchErrorTag = "EtagMismatchError"
)

type EtagMismatchError struct {
	currentEtag string
	latestEtag  string
}

func NewEtagMismatchError(httpStatusCode int, respBody string) *EtagMismatchError {
	if httpStatusCode != http.StatusPreconditionFailed {
		return nil
	}

	currentEtag, latestEtag, match := getMatchedLatestAndCurrentEtags(respBody)
	if !match {
		return nil
	}

	return &EtagMismatchError{
		currentEtag: currentEtag,
		latestEtag:  latestEtag,
	}
}

func (e *EtagMismatchError) Error() string {
	return fmt.Sprintf("%s: etag %s does not match etag %s of resource", EtagMismatchErrorTag, e.currentEtag, e.latestEtag)
}

func (e *EtagMismatchError) CurrentEtag() string {
	return e.currentEtag
}

func (e *EtagMismatchError) LatestEtag() string {
	return e.latestEtag
}

func (e *EtagMismatchError) Is(target error) bool {
	return strings.Contains(target.Error(), EtagMismatchErrorTag)
}

// isPreconditionFailedEtagMismatch returns true the if the request failed for Etag mismatch
func IsPreconditionFailedEtagMismatch(httpStatusCode int, respBody string) bool {

	if httpStatusCode != http.StatusPreconditionFailed {
		return false
	}

	_, _, match := getMatchedLatestAndCurrentEtags(respBody)
	return match
}

func getMatchedLatestAndCurrentEtags(respBody string) (string, string, bool) {

	var currentEtag, latestEtag string
	re := regexp.MustCompile(EtagMismatchPattern)
	matches := re.FindStringSubmatch(respBody)

	if len(matches) != 3 {
		return currentEtag, latestEtag, false
	}

	currentEtag = matches[1]
	latestEtag = matches[2]

	return currentEtag, latestEtag, true
}
