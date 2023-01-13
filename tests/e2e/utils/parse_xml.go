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

package utils

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
)

type testsuites struct {
	XMLName     xml.Name    `xml:"testsuites"`
	Version     string      `xml:"version,attr"`
	Description string      `xml:",innerxml"`
	TestSuites  []testsuite `xml:"testsuite"`
}

type testsuite struct {
	XMLName   xml.Name   `xml:"testsuite"`
	Testcases []testcase `xml:"testcase"`
}

// Here we only care about failed testcases, not succeeded or skipped ones.
type testcase struct {
	XMLName xml.Name `xml:"testcase"`
	Name    string   `xml:"name,attr"`
	Failure failure  `xml:"failure"`
}

type failure struct {
	XMLName xml.Name `xml:"failure"`
	Message string   `xml:"message,attr"`
}

// parseXML parses the XML file and return a string including only failed cases in XML.
func parseXML(junitReportPath string) (string, error) {
	file, err := os.Open(junitReportPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file %q: %w", junitReportPath, err)
	}
	defer file.Close()
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	v := testsuites{}
	if err := xml.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("failed to unmarshal XML data: %w", err)
	}
	if len(v.TestSuites) == 0 {
		return "", fmt.Errorf("xml file is empty: %s", string(data))
	}

	failedTestcases := []testcase{}
	for _, tc := range v.TestSuites[0].Testcases {
		if tc.Failure.Message != "" {
			failedTestcases = append(failedTestcases, tc)
		}
	}

	output, err := xml.MarshalIndent(failedTestcases, "  ", "    ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal test result of failed ones")
	}

	report := strings.ReplaceAll(string(output), ",", " ")
	return report, nil
}
