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
	"sigs.k8s.io/controller-tools/pkg/markers"
)

func generateTest(ctx *genall.GenerationContext, root *loader.Package, headerText string) error {
	if err := generateTestSuite(ctx, root, headerText); err != nil {
		return err
	}

	if err := generateTestCase(ctx, root, headerText); err != nil {
		return err
	}

	if err := generateTestCaseCustom(ctx, root, headerText); err != nil {
		return err
	}

	if err := os.MkdirAll(root.Name+"/testdata", 0755); err != nil {
		return err
	}
	fmt.Println("Generated test in " + root.Name)
	return nil
}

func generateTestSuite(ctx *genall.GenerationContext, root *loader.Package, headerText string) error {
	var codeSnips []*bytes.Buffer
	var importList = make(map[string]map[string]struct{})
	err := markers.EachType(ctx.Collector, root, func(typeInfo *markers.TypeInfo) {
		if typeInfo := typeInfo.Markers.Get(clientGenMarker.Name); typeInfo != nil {
			markerConf := typeInfo.(ClientGenConfig)
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
			}
			codeSnips = append(codeSnips, &outContent)
		}
	})
	if err != nil {
		root.AddError(err)
		return err
	}
	if len(codeSnips) <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	importList["testing"] = make(map[string]struct{})
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/recording"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
	importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{".": {}}
	importList["github.com/onsi/gomega"] = map[string]struct{}{".": {}}

	return WriteToFile(ctx, root, root.Name+"_suite_test.go", headerText, importList, codeSnips)
}

func generateTestCase(ctx *genall.GenerationContext, root *loader.Package, headerText string) error {
	var importList = make(map[string]map[string]struct{})

	var codeSnips []*bytes.Buffer
	err := markers.EachType(ctx.Collector, root, func(typeInfo *markers.TypeInfo) {
		if typeInfo := typeInfo.Markers.Get(clientGenMarker.Name); typeInfo != nil {
			markerConf := typeInfo.(ClientGenConfig)

			aliasMap, ok := importList[markerConf.PackageName]
			if !ok {
				aliasMap = make(map[string]struct{})
				importList[markerConf.PackageName] = aliasMap
			}
			aliasMap[markerConf.PackageAlias] = struct{}{}

			var outContent bytes.Buffer
			if err := TestCaseTemplate.Execute(&outContent, markerConf); err != nil {
				root.AddError(err)
			}
			codeSnips = append(codeSnips, &outContent)
		}
	})
	if err != nil {
		root.AddError(err)
		return err
	}
	if len(codeSnips) <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/to"] = make(map[string]struct{})
	importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{".": {}}
	importList["github.com/onsi/gomega"] = map[string]struct{}{".": {}}
	return WriteToFile(ctx, root, root.Name+"_test.go", headerText, importList, codeSnips)
}

func generateTestCaseCustom(ctx *genall.GenerationContext, root *loader.Package, headerText string) error {
	if _, err := os.Lstat(root.Name + "/custom_test.go"); err == nil || !os.IsNotExist(err) {
		fmt.Println(root.Name + "/custom_test.go already exists, skipping generation of custom test")
		return nil
	}

	var importList = make(map[string]map[string]struct{})
	var codeSnips []*bytes.Buffer
	err := markers.EachType(ctx.Collector, root, func(typeInfo *markers.TypeInfo) {
		if typeInfo := typeInfo.Markers.Get(clientGenMarker.Name); typeInfo != nil {
			markerConf := typeInfo.(ClientGenConfig)

			aliasMap, ok := importList[markerConf.PackageName]
			if !ok {
				aliasMap = make(map[string]struct{})
				importList[markerConf.PackageName] = aliasMap
			}
			aliasMap[markerConf.PackageAlias] = struct{}{}

			var outContent bytes.Buffer
			if err := TestCaseCustomTemplate.Execute(&outContent, markerConf); err != nil {
				root.AddError(err)
			}
			codeSnips = append(codeSnips, &outContent)
		}
	})
	if err != nil {
		root.AddError(err)
		return err
	}
	if len(codeSnips) <= 0 {
		return nil
	}

	importList["context"] = make(map[string]struct{})
	return WriteToFile(ctx, root, "custom_test.go", headerText, importList, codeSnips)
}
