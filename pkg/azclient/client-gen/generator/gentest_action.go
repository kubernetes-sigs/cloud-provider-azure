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

// Package generator
package generator

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
)

func generateTest(ctx *genall.GenerationContext, root *loader.Package, typeName string, markerConf ClientGenConfig, headerText string) error {
	if err := generateTestSuite(ctx, root, typeName, markerConf, headerText); err != nil {
		return err
	}

	if err := generateTestCase(ctx, root, typeName, markerConf, headerText); err != nil {
		return err
	}

	if err := generateTestCaseCustom(ctx, root, typeName, markerConf, headerText); err != nil {
		return err
	}

	if err := os.MkdirAll(root.Name+"/testdata", 0755); err != nil {
		return err
	}
	fmt.Println("Generated test in " + root.Name)
	return nil
}

func generateTestSuite(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig, headerText string) error {
	var importList = make(map[string]map[string]struct{})

	for _, verb := range markerConf.Verbs {
		if strings.EqualFold(FuncCreateOrUpdate, verb) {
			if markerConf.SubResource == "" {
				importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/to"] = make(map[string]struct{})
			}
		}
	}
	var outContent bytes.Buffer
	if err := TestSuiteTemplate.Execute(&outContent, markerConf); err != nil {
		root.AddError(err)
		return err
	}

	if outContent.Len() <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	importList["testing"] = make(map[string]struct{})
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/recording"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/to"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
	importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{}
	importList["github.com/onsi/gomega"] = map[string]struct{}{}
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})

	return WriteToFile(ctx, root, root.Name+"_suite_test.go", headerText, importList, &outContent)
}

func generateTestCase(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig, headerText string) error {
	var importList = make(map[string]map[string]struct{})
	for _, verb := range markerConf.Verbs {
		if strings.EqualFold(FuncCreateOrUpdate, verb) {
			aliasMap := make(map[string]struct{})
			aliasMap[markerConf.PackageAlias] = struct{}{}
			importList[markerConf.PackageName] = aliasMap
			importList["strings"] = make(map[string]struct{})
		}
	}
	if len(markerConf.Verbs) > 0 {
		importList["github.com/onsi/gomega"] = map[string]struct{}{}
	}
	var outContent bytes.Buffer
	if err := TestCaseTemplate.Execute(&outContent, markerConf); err != nil {
		root.AddError(err)
	}

	if outContent.Len() <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{}
	return WriteToFile(ctx, root, root.Name+"_test.go", headerText, importList, &outContent)
}

func generateTestCaseCustom(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig, headerText string) error {
	if _, err := os.Lstat(root.Name + "/custom_test.go"); err == nil || !os.IsNotExist(err) {
		fmt.Println(root.Name + "/custom_test.go already exists, skipping generation of custom test")
		return nil
	}

	var importList = make(map[string]map[string]struct{})
	aliasMap := make(map[string]struct{})
	aliasMap[markerConf.PackageAlias] = struct{}{}
	importList[markerConf.PackageName] = aliasMap

	var outContent bytes.Buffer
	if err := TestCaseCustomTemplate.Execute(&outContent, markerConf); err != nil {
		root.AddError(err)
	}

	if outContent.Len() <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	return WriteToFile(ctx, root, "custom_test.go", headerText, importList, &outContent)
}
