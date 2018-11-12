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

	"github.com/golang/glog"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"

	_ "k8s.io/cloud-provider-azure/tests/e2e/autoscaling"
	_ "k8s.io/cloud-provider-azure/tests/e2e/network"
)

func TestAzureTest(t *testing.T) {
	RegisterFailHandler(Fail)
	reportDir := "_report/"

	var r []Reporter
	if reportDir != "" {
		if err := os.MkdirAll(reportDir, 0755); err != nil {
			glog.Fatalf("Failed creating report directory: %v", err)
		} else {
			r = append(r, reporters.NewJUnitReporter(path.Join(reportDir, fmt.Sprintf("junit_%02d.xml", config.GinkgoConfig.ParallelNode))))
		}
	}
	RunSpecsWithDefaultAndCustomReporters(t, "Cloud provider Azure e2e suite", r)
}
