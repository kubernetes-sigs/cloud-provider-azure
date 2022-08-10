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

package e2e

import (
	"fmt"
	"os"
	"path"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/klog/v2"

	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/auth"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/autoscaling"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/network"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/node"
)

const (
	reportDirEnv     = "CCM_JUNIT_REPORT_DIR"
	artifactsDirEnv  = "ARTIFACTS"
	defaultReportDir = "_report/"
)

func TestAzureTest(t *testing.T) {
	RegisterFailHandler(Fail)
	reportDir := os.Getenv(reportDirEnv)
	if reportDir == "" {
		artifactsDir := os.Getenv(artifactsDirEnv)
		if artifactsDir != "" {
			reportDir = artifactsDir
		} else {
			reportDir = defaultReportDir
		}
	}
	if reportDir != "" {
		if err := os.MkdirAll(reportDir, 0755); err != nil {
			klog.Fatalf("Failed creating report directory: %v", err)
		}
	}

	suiteConfig, reporterConfig := GinkgoConfiguration()
	suiteConfig.Timeout = 0
	reporterConfig.Verbose = true
	reporterConfig.JUnitReport = path.Join(reportDir, fmt.Sprintf("junit_%02d.xml", GinkgoParallelProcess()))
	RunSpecs(t, "Cloud provider Azure e2e suite", suiteConfig, reporterConfig)
}
