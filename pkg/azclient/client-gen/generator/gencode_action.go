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
	"os/exec"
	"strings"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

// generateClient writes out the build tag, package declaration, and imports
func generateClient(ctx *genall.GenerationContext, root *loader.Package, headerText string) error {
	var importList = make(map[string]map[string]struct{})
	var codeSnips []*bytes.Buffer
	err := markers.EachType(ctx.Collector, root, func(typeInfo *markers.TypeInfo) {
		if typeInfo := typeInfo.Markers.Get(clientGenMarker.Name); typeInfo != nil {
			markerConf := typeInfo.(ClientGenConfig)
			//nolint:gosec // G204 ignore this!
			cmd := exec.Command("go", "get", "-v", markerConf.PackageName)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				root.AddError(err)
				return
			}

			aliasMap, ok := importList[markerConf.PackageName]
			if !ok {
				aliasMap = make(map[string]struct{})
				importList[markerConf.PackageName] = aliasMap
			}
			aliasMap[markerConf.PackageAlias] = struct{}{}

			var outContent bytes.Buffer
			if err := ClientTemplate.Execute(&outContent, markerConf); err != nil {
				root.AddError(err)
			}
			if err := ClientFactoryTemplate.Execute(&outContent, markerConf); err != nil {
				root.AddError(err)
			}

			// define structs
			for _, verb := range markerConf.Verbs {
				switch true {
				case strings.EqualFold(FuncCreateOrUpdate, verb):
					if err := CreateOrUpdateFuncTemplate.Execute(&outContent, markerConf); err != nil {
						root.AddError(err)
					}
				case strings.EqualFold(FuncDelete, verb):
					if err := DeleteFuncTemplate.Execute(&outContent, markerConf); err != nil {
						root.AddError(err)
					}
				case strings.EqualFold(FuncListByRG, verb):
					if err := ListByRGFuncTemplate.Execute(&outContent, markerConf); err != nil {
						root.AddError(err)
					}
				case strings.EqualFold(FuncList, verb):
					if err := ListFuncTemplate.Execute(&outContent, markerConf); err != nil {
						root.AddError(err)
					}
				case strings.EqualFold(FuncGet, verb):
					if err := GetFuncTemplate.Execute(&outContent, markerConf); err != nil {
						root.AddError(err)
					}
				}
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
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
	if err = WriteToFile(ctx, root, "zz_generated_client.go", headerText, importList, codeSnips); err != nil {
		return err
	}
	fmt.Println("Generated zz_generated_client.go in " + root.Name)
	return nil
}
