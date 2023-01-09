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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/ingest"
	"k8s.io/klog/v2"
)

var (
	tenantID     = os.Getenv("AZURE_TENANT_ID")
	clientID     = os.Getenv("AZURE_CLIENT_ID")
	clientSecret = os.Getenv("AZURE_CLIENT_SECRET")

	ingestionURI = os.Getenv("KUSTO_INGESTION_URI")
	branchName   = os.Getenv("BUILD_SOURCE_BRANCH_NAME")
	database     = "upstreampipeline"
	table        = "TestResult"
)

// validate checks if necessary info is provided.
func validate() error {
	emptyVar := ""
	if tenantID == "" {
		emptyVar = tenantID
	}
	if clientID == "" {
		emptyVar = clientID
	}
	if clientSecret == "" {
		emptyVar = clientSecret
	}
	if ingestionURI == "" {
		emptyVar = ingestionURI
	}
	if emptyVar == "" {
		return nil
	}
	return fmt.Errorf("environment variable %q is empty for kusto ingestion", emptyVar)
}

// KustoIngest ingests test result to kusto
// The table is like:
// | Timestamp            | TestScenario                            | ClusterType | BranchName   | Passed | ErrorDetails |
// | 2023-01-09T01:00:00Z | Feature:Autoscaling || !Serial && !Slow | autoscaling | release-1.26 | true   | <failed-tests-detail> |
func KustoIngest(passed bool, labelFilter, clusterType, junitReportPath string) error {
	if err := validate(); err != nil {
		return err
	}

	kustoConnStrBuilder := kusto.NewConnectionStringBuilder(ingestionURI).WithAadAppKey(clientID, clientSecret, tenantID)
	kustoClient, err := kusto.New(kustoConnStrBuilder)
	if err != nil {
		return fmt.Errorf("failed to new kusto connection string builder: %w", err)
	}
	defer kustoClient.Close()

	in, err := ingest.New(kustoClient, database, table)
	if err != nil {
		return fmt.Errorf("failed to new ingestion: %w", err)
	}
	defer in.Close()

	// Ingesting
	r, w := io.Pipe()

	enc := json.NewEncoder(w)
	go func() {
		defer w.Close()

		currentTime := time.Now().Format(time.RFC3339)
		passedStr := "false"
		if passed {
			passedStr = "true"
		}

		report, err := parseXML(junitReportPath)
		if err != nil {
			panic(fmt.Errorf("failed to parse XML file: %w", err))
		}

		data := fmt.Sprintf(`"Timestamp":%s,%s,%s,%s,%s,%s,`,
			currentTime, labelFilter, clusterType, branchName, passedStr, report)
		if err := enc.Encode(data); err != nil {
			panic(fmt.Errorf("failed to json encode data: %w", err))
		}
		klog.Infof("ingested data: %s", data)
	}()

	if _, err := in.FromReader(context.Background(), r); err != nil {
		return fmt.Errorf("failed to ingest from reader: %w", err)
	}
	return nil
}
