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

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
)

type TypeScaffoldOptions struct {
	Resource     string
	Package      string
	PackageAlias string
	ClientName   string
	Verbs        []string
	Expand       bool
}

var (
	TypeTemplateRaw = `
/*
Copyright {{now.UTC.Year}} The Kubernetes Authors.

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

// +azure:enableclientgen:=true
package {{tolower .Resource}}client
import (
	{{tolower .PackageAlias}} "{{.Package}}"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

// +azure:client:verbs={{join .Verbs ";"}},resource={{.Resource}},packageName={{.Package}},packageAlias={{tolower .PackageAlias}},clientName={{.ClientName}},expand={{.Expand}}
type Interface interface {
{{ $expandable := .Expand}}
{{ $packageAlias := .PackageAlias}}
{{ $resource := .Resource}}
{{range $index,$element := .Verbs}}
{{if strequal $element "Get"}}
    {{ if $expandable }}utils.GetWithExpandFunc[{{tolower $packageAlias}}.{{$resource}}]{{ else }}utils.GetFunc[{{tolower $packageAlias}}.{{$resource}}]{{end}}{{end}}
{{if or (strequal $element "ListByRG") (strequal $element "List") }}
	utils.ListFunc[{{tolower $packageAlias}}.{{$resource}}]{{end}}
{{if strequal $element "CreateOrUpdate"}}
	utils.CreateOrUpdateFunc[{{tolower $packageAlias}}.{{$resource}}]{{end}}
{{if strequal $element "Delete"}}
	utils.DeleteFunc[{{tolower $packageAlias}}.{{$resource}}]{{end}}
{{end}}
}
`
	typesTemplateHelpers = template.FuncMap{
		"tolower":  strings.ToLower,
		"now":      time.Now,
		"join":     strings.Join,
		"strequal": strings.EqualFold,
	}

	typesTemplate = template.Must(template.New("object-scaffolding").Funcs(typesTemplateHelpers).Parse(TypeTemplateRaw))
)

func main() {
	scaffoldOptions := TypeScaffoldOptions{}

	// rootCmd represents the base command when called without any subcommands
	var rootCmd = &cobra.Command{
		Use:   "typescaffold",
		Short: "scaffold client interface for azure resource",
		Long:  `scaffold client interface for azure resource`,

		Run: func(cmd *cobra.Command, args []string) {

			content := new(bytes.Buffer)
			if err := typesTemplate.Execute(content, scaffoldOptions); err != nil {
				fmt.Printf("failed to generate client from template %s\n", err.Error())
				return
			}
			formattedContent, err := format.Source(content.Bytes())
			if err != nil {
				fmt.Printf("failed to generate client %s\n", err.Error())
				return
			}
			fileName := strings.ToLower(scaffoldOptions.Resource) + "client"
			if err := os.MkdirAll(fileName, 0755); err != nil && !os.IsExist(err) {
				fmt.Printf("failed to create dir %s\n", err.Error())
				return
			}
			if _, err = os.Lstat(fileName + "/interface.go"); err == nil {
				fmt.Printf("file %s already exists, skip\n", fileName+"/interface.go")
				return
			}
			err = os.WriteFile(fileName+"/interface.go", formattedContent, 0600)
			if err != nil {
				fmt.Printf("failed to write file %s\n", err.Error())
				return
			}
		},
	}

	rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	rootCmd.Flags().StringVar(&scaffoldOptions.Package, "package", "", "resource package")
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "package"); err != nil {
		fmt.Printf("failed to mark package option as required, %s\n", err)
		return
	}

	rootCmd.Flags().StringVar(&scaffoldOptions.PackageAlias, "package-alias", "", "resource package alias")
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "package-alias"); err != nil {
		fmt.Printf("failed to mark package-alias option as required, %s\n", err)
		return
	}
	rootCmd.Flags().StringVar(&scaffoldOptions.Resource, "resource", "", "resource name")
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "resource"); err != nil {
		fmt.Printf("failed to mark resource option as required, %s\n", err)
		return
	}

	rootCmd.Flags().StringVar(&scaffoldOptions.ClientName, "client-name", "", "client name")
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "client-name"); err != nil {
		fmt.Printf("failed to mark resource option as required, %s\n", err)
		return
	}
	rootCmd.Flags().StringSliceVar(&scaffoldOptions.Verbs, "verbs", []string{"get", "createorupdate", "delete", "listbyrg"}, "verbs")
	rootCmd.Flags().BoolVar(&scaffoldOptions.Expand, "expand", false, "get support expand params")
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
