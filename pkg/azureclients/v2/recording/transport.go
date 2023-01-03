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

package recording

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/dnaeon/go-vcr.v2/cassette"
	"gopkg.in/dnaeon/go-vcr.v2/recorder"
)

func translateErrors(r *recorder.Recorder, cassetteName string) http.RoundTripper {
	return errorTranslation{r, cassetteName, nil}
}

type errorTranslation struct {
	recorder     *recorder.Recorder
	cassetteName string

	cassette *cassette.Cassette
}

func (w errorTranslation) ensureCassette() *cassette.Cassette {
	if w.cassette == nil {
		cassette, err := cassette.Load(w.cassetteName)
		if err != nil {
			panic(fmt.Sprintf("unable to load cassette %q", w.cassetteName))
		}

		w.cassette = cassette
	}

	return w.cassette
}

func (w errorTranslation) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, originalErr := w.recorder.RoundTrip(req)
	// sorry, go-vcr doesn't expose the error type or message
	if originalErr == nil || !strings.Contains(originalErr.Error(), "interaction not found") {
		return resp, originalErr
	}

	sentBodyString := "<nil>"
	if req.Body != nil {
		bodyBytes, bodyErr := io.ReadAll(req.Body)
		if bodyErr != nil {
			// see invocation of SetMatcher in the createRecorder, which does this
			panic("io.ReadAll(req.Body) failed, this should always succeed because req.Body has been replaced by a buffer")
		}

		// Apply the same body filtering that we do in recordings so that the diffs don't show things
		// that we've just removed
		sentBodyString = hideRecordingData(string(bodyBytes))
	}

	// find all request bodies for the specified method/URL combination
	matchingBodies := w.findMatchingBodies(req)

	if len(matchingBodies) == 0 {
		return nil, fmt.Errorf("cannot find go-vcr recording for request (cassette: %q) (no responses recorded for this method/URL): %s %s (attempt: %s)\n\n",
			w.cassetteName,
			req.Method,
			req.URL.String(),
			req.Header.Get(countHeader))
	}

	// locate the request body with the shortest diff from the sent body
	shortestDiff := ""
	for i, bodyString := range matchingBodies {
		diff := cmp.Diff(bodyString, sentBodyString)
		if i == 0 || len(diff) < len(shortestDiff) {
			shortestDiff = diff
		}
	}

	return nil, fmt.Errorf("cannot find go-vcr recording for request (cassette: %q) (body mismatch): %s %s\nShortest body diff: %s\n\n",
		w.cassetteName,
		req.Method,
		req.URL.String(),
		shortestDiff)
}

// finds bodies for interactions where request method, URL, and countHeader match
func (w errorTranslation) findMatchingBodies(r *http.Request) []string {
	urlString := r.URL.String()
	var result []string
	for _, interaction := range w.ensureCassette().Interactions {
		if urlString == interaction.URL && r.Method == interaction.Request.Method &&
			r.Header.Get(countHeader) == interaction.Request.Headers.Get(countHeader) {
			result = append(result, interaction.Request.Body)
		}
	}

	return result
}
