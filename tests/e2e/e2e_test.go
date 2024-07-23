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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/klog/v2"

	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/auth"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/autoscaling"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/network"
	_ "sigs.k8s.io/cloud-provider-azure/tests/e2e/node"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

const (
	reportDirEnv                = "CCM_JUNIT_REPORT_DIR"
	artifactsDirEnv             = "ARTIFACTS"
	defaultReportDir            = "_report/"
	clusterProvisioningToolKey  = "CLUSTER_PROVISIONING_TOOL"
	clusterProvisioningToolCAPZ = "capz"
	testMultiSLB                = "TEST_MULTI_SLB"
	healthProbeMode             = "HEALTH_PROBE_MODE"
	failFast                    = "FAIL_FAST"
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

	if os.Getenv(failFast) == utils.TrueValue {
		suiteConfig.FailFast = true
	}

	labelFilters := []string{}
	if suiteConfig.LabelFilter != "" {
		labelFilters = append(labelFilters, suiteConfig.LabelFilter)
	}

	if strings.EqualFold(os.Getenv(clusterProvisioningToolKey), clusterProvisioningToolCAPZ) {
		labelFilters = append(labelFilters, "!SLBOutbound")
	}

	var multiSLBFilter string
	if !strings.EqualFold(os.Getenv(testMultiSLB), utils.TrueValue) {
		multiSLBFilter = "!Multi-SLB"
	} else {
		multiSLBFilter = "!Non-Multi-Slb"
	}
	labelFilters = append(labelFilters, multiSLBFilter)

	var sharedProbeFilter string
	if strings.EqualFold(os.Getenv(healthProbeMode), "shared") {
		sharedProbeFilter = "Shared-Health-Probe"
	} else {
		sharedProbeFilter = "!Shared-Health-Probe"
	}
	labelFilters = append(labelFilters, sharedProbeFilter)

	suiteConfig.LabelFilter = strings.Join(labelFilters, " && ")

	reporterConfig.Verbose = true
	reporterConfig.JUnitReport = path.Join(reportDir, fmt.Sprintf("junit_%02d.xml", GinkgoParallelProcess()))
	passed := RunSpecs(t, "Cloud provider Azure e2e suite", suiteConfig, reporterConfig)

	// If it is the test result of the upstream AKS pipeline, it should be ingested to kusto.
	if !strings.EqualFold(os.Getenv(utils.IngestTestResult), utils.TrueValue) {
		return
	}

	// TODO: Use managed identity for Kusto ingestion
	skipKustoIngestion := true
	if skipKustoIngestion {
		return
	}

	klog.Infof("Ingesting test result to kusto")
	if err := utils.KustoIngest(passed, suiteConfig.LabelFilter, os.Getenv(utils.AKSClusterType), reporterConfig.JUnitReport); err != nil {
		klog.Error(err)
	}
}
