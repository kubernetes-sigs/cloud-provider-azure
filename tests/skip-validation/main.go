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

package main

import (
	"fmt"
	"log"
	"os"

	valid "k8s.io/cloud-provider-azure/tests/skip-validation/validation"
)

const (
	yamlPath    = "tests/skip-validation/"
	skipTxtPath = "tests/k8s-azure/"
)

func main() {
	skipDescriptions, err := valid.ReadSkipFile(yamlPath)
	if err != nil {
		err = valid.GenerateSkipFile(yamlPath, skipTxtPath)
		if err != nil {
			log.Fatal(err)
		}
		skipDescriptions, err = valid.ReadSkipFile(yamlPath)
		if err != nil {
			log.Fatal(err)
		}
	}
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		log.Fatal(fmt.Errorf("Environment variable CLUSTER_NAME is empty"))
	}
	reportPath := fmt.Sprintf("./%s/", clusterName)
	resultList, err := valid.BuildTempSkipsLock(skipDescriptions, reportPath, yamlPath)
	if err != nil {
		log.Fatal(err)
	}
	if !valid.CheckTest(skipDescriptions, resultList) {
		log.Fatal(fmt.Errorf("Unmatched skip.lock.yaml and temp.log.yaml, please mannually check them in %s", yamlPath))
	}
}
