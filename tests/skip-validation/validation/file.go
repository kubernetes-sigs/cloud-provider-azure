/*
Copyright 2018 The Kubernetes Authors.

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

package validation

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
)

const (
	lockFile = "skip.lock.yaml"
	tempLock = "temp.lock.yaml"
	skipFile = "skip.txt"
)

func extractSkipKey(filename string) (result []Description, err error) {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		return
	}
	text := string(bytes)
	lines := strings.Split(text, "\r\n")

	var testClass Description

	for _, line := range lines {
		if line == "" {
			testClass.Comment = ""
		} else if line[0] != '#' { // Name
			testClass.Name = line
			result = append(result, testClass)

			testClass.Subtest = make([]string, 0)
		} else if line[1] == ' ' || line[1] == '#' { // comment
			testClass.Comment += line[2:]
		} else { // sub test
			testClass.Subtest = append(testClass.Subtest, line[1:])
		}
	}
	return
}

// GenerateSkipFile generates skip.log.yaml according to skip.txt
func GenerateSkipFile(lockDir string, skipDir string) (err error) {
	lockfile := lockDir + lockFile
	descrips, err := extractSkipKey(skipDir + skipFile)
	if err != nil {
		return err
	}
	suiteYaml, _ := yaml.Marshal(descrips)
	ioutil.WriteFile(lockfile, suiteYaml, 0644)
	return
}

// ReadSkipFile extract Skip descriptions from skip.lock.yaml
func ReadSkipFile(file string) (result []Description, err error) {
	yamlFile, err := os.Open(file + lockFile)
	if err != nil {
		return
	}
	defer yamlFile.Close()
	byteValue, err := ioutil.ReadAll(yamlFile)
	if err != nil {
		return
	}
	yaml.Unmarshal(byteValue, &result)
	return
}

// getReportList extracts all the tests from junit reports
func getReportList(dirPath string) (reportList []string, err error) {
	reportList = make([]string, 0)

	updateReportFromJunit := func(file string) (err error) {
		xmlFile, err := os.Open(file)
		if err != nil {
			return err
		}
		defer xmlFile.Close()
		byteValue, err := ioutil.ReadAll(xmlFile)
		if err != nil {
			return err
		}
		var suite TestSuite
		xml.Unmarshal(byteValue, &suite)
		for _, tc := range suite.TestCases {
			reportList = append(reportList, tc.Name)
		}
		return
	}

	visitJunitReport := func(fp string, fi os.FileInfo, err error) error {
		if err != nil {
			fmt.Println(err) // can't walk here,
			return nil       // but continue walking elsewhere
		}
		if fi.IsDir() {
			return nil // not a file.  ignore.
		}
		matched, err := filepath.Match("junit_*.xml", fi.Name())
		if err != nil {
			fmt.Println(err) // malformed pattern
			return err       // this is fatal.
		}
		if matched {
			err = updateReportFromJunit(fp)
			if err != nil {
				fmt.Println(err) // fail to read certain file, but continue
				return nil
			}
		}
		return nil
	}
	err = filepath.Walk(dirPath, visitJunitReport)
	if len(reportList) == 0 {
		err = fmt.Errorf("No reports in target directoy")
	}
	return
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// BuildTempSkipsLock reads report and generates temp.lock.yaml
func BuildTempSkipsLock(skipFromFile []Description, reportPath string, storeDir string) (result []Description, err error) {
	reportList, err := getReportList(reportPath)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(skipFromFile); i++ {
		temp := Description{
			skipFromFile[i].Name,
			skipFromFile[i].Comment,
			make([]string, 0),
		}
		for j := 0; j < len(reportList); j++ {
			if strings.Contains(reportList[j], skipFromFile[i].Name) {
				if !contains(temp.Subtest, reportList[j]) {
					temp.Subtest = append(temp.Subtest, reportList[j])
				}
			}
		}
		result = append(result, temp)
	}
	suiteYaml, err := yaml.Marshal(result)
	if err != nil {
		return
	}
	ioutil.WriteFile(storeDir+tempLock, suiteYaml, 0644)
	return
}
