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
	"strings"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
)

// generateClient writes out the build tag, package declaration, and imports
func (g Generator) generateClient(ctx *genall.GenerationContext, root *loader.Package, _ string, markerConf ClientGenConfig) error {
	var importList = make(map[string]map[string]struct{})
	aliasMap := make(map[string]struct{})
	aliasMap[markerConf.PackageAlias] = struct{}{}
	importList[markerConf.PackageName] = aliasMap
	importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
	importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/tracing"] = make(map[string]struct{})
	if markerConf.OutOfSubscriptionScope && len(markerConf.Verbs) > 0 {
		importList["go.opentelemetry.io/otel/attribute"] = make(map[string]struct{})
	}
	if markerConf.Etag {
		importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})
		importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/etag"] = make(map[string]struct{})
	}

	file, err := ctx.Open(root, "zz_generated_client.go")
	if err != nil {
		return err
	}
	defer file.Close()

	if len(markerConf.Verbs) > 0 {
		importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"] = make(map[string]struct{})
		importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"] = make(map[string]struct{})
		importList["context"] = make(map[string]struct{})
	}

	if err := DumpHeaderToWriter(ctx, file, g.HeaderFile, importList, root.Name); err != nil {
		return err
	}

	if err := ClientTemplate.Execute(file, markerConf); err != nil {
		root.AddError(err)
		return err
	}
	if err := ClientFactoryTemplate.Execute(file, markerConf); err != nil {
		root.AddError(err)
		return err
	}
	// define structs
	for _, verb := range markerConf.Verbs {
		switch true {
		case strings.EqualFold(FuncCreateOrUpdate, verb):
			if err := CreateOrUpdateFuncTemplate.Execute(file, markerConf); err != nil {
				root.AddError(err)
				return err
			}
		case strings.EqualFold(FuncDelete, verb):
			if err := DeleteFuncTemplate.Execute(file, markerConf); err != nil {
				root.AddError(err)
				return err
			}
		case strings.EqualFold(FuncListByRG, verb):
			if err := ListByRGFuncTemplate.Execute(file, markerConf); err != nil {
				root.AddError(err)
				return err
			}
		case strings.EqualFold(FuncList, verb):
			if err := ListFuncTemplate.Execute(file, markerConf); err != nil {
				root.AddError(err)
				return err
			}
		case strings.EqualFold(FuncGet, verb):
			if err := GetFuncTemplate.Execute(file, markerConf); err != nil {
				root.AddError(err)
				return err
			}
		}
	}

	return nil
}
