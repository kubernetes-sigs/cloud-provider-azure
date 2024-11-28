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
	"go/ast"
	"os"
	"os/exec"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

type MockGenerator struct {
	HeaderFile string `marker:",optional"`
}

func (MockGenerator) RegisterMarkers(into *markers.Registry) error {
	return markers.RegisterAll(into, clientGenMarker, enableClientGenMarker)
}

func (generator MockGenerator) Generate(ctx *genall.GenerationContext) error {
	cmd := exec.Command("go", "get", "go.uber.org/mock/mockgen/model")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	var err error
	if err = cmd.Run(); err != nil {
		return err
	}
	for _, root := range ctx.Roots {
		pkgMakers, err := markers.PackageMarkers(ctx.Collector, root)
		if err != nil {
			root.AddError(err)
			break
		}
		if _, markedForGeneration := pkgMakers[enableClientGenMarker.Name]; !markedForGeneration {

			continue
		}

		//visit each type
		root.NeedTypesInfo()

		err = markers.EachType(ctx.Collector, root, func(typeInfo *markers.TypeInfo) {
			marker := typeInfo.Markers.Get(clientGenMarker.Name)
			if marker == nil {
				return
			}

			mockFile, err := ctx.OutputRule.Open(root, "mock_"+root.Name+"/interface.go")
			if err != nil {
				root.AddError(err)
				return
			}
			defer mockFile.Close()
			//nolint:gosec // G204 ignore this!
			cmd := exec.Command("mockgen", "-package", "mock_"+root.Name, "-source", root.Name+"/interface.go", "-copyright_file", generator.HeaderFile)
			cmd.Stdout = mockFile
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				root.AddError(err)
				return
			}

		})
		if err != nil {
			break
		}
	}
	return err
}

func (generator MockGenerator) Help() *markers.DefinitionHelp {
	return &markers.DefinitionHelp{
		DetailedHelp: markers.DetailedHelp{
			Summary: "Generate mock for the given package",
			Details: `Generate mock for the given package`,
		},
		FieldHelp: map[string]markers.DetailedHelp{
			"HeaderFile": {
				Summary: "header file path",
				Details: `header file path`,
			},
		},
	}
}

func (MockGenerator) CheckFilter() loader.NodeFilter {
	return func(node ast.Node) bool {
		// ignore structs
		_, isIface := node.(*ast.InterfaceType)
		return isIface
	}
}
