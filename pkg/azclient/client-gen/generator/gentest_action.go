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
	"strings"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
)

func (g Generator) generateTest(ctx *genall.GenerationContext, root *loader.Package, typeName string, markerConf ClientGenConfig) error {
	if err := g.generateTestSuite(ctx, root, typeName, markerConf); err != nil {
		return err
	}

	if err := g.generateTestCase(ctx, root, typeName, markerConf); err != nil {
		return err
	}

	return nil
}

func (g Generator) generateTestSuite(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig) error {
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
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"] = make(map[string]struct{})
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})

	// return DumpHeaderToWriter(ctx, root, root.Name+"_suite_test.go", headerText, importList, &outContent)
	return nil
}

func (g Generator) generateTestCase(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig) error {
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
	importList["context"] = make(map[string]struct{})
	importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{}
	if markerConf.Etag {
		importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/to"] = make(map[string]struct{})
	}
	file, err := ctx.Open(root, root.Name+"_test.go")
	if err != nil {
		return err
	}
	defer file.Close()

	if err := DumpHeaderToWriter(ctx, file, g.HeaderFile, importList, root.Name); err != nil {
		return err
	}
	if err := TestCaseTemplate.Execute(file, markerConf); err != nil {
		root.AddError(err)
	}
	return nil
}
