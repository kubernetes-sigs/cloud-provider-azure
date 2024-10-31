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
	Resource        string
	SubResource     string
	Package         string
	PackageAlias    string
	ClientName      string
	PropertyName    string
	Verbs           []string
	Expand          bool
	RateLimitKey    string
	CrossSubFactory bool
	Etag            bool
}

var (
	TypeTemplateHeader = `
{{ $resource := .Resource}}
{{ if (gt (len .SubResource) 0) }}
{{ $resource = .SubResource}}
{{ end }}
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
package {{tolower $resource}}client
import (
	{{tolower .PackageAlias}} "{{.Package}}"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)
`
	TypeResourceTemplate = `
// +azure:client:verbs={{join .Verbs ";"}},resource={{.Resource}},packageName={{.Package}},packageAlias={{tolower .PackageAlias}},clientName={{.ClientName}},expand={{.Expand}}{{with .RateLimitKey}},rateLimitKey={{.}}{{end}}{{with .Etag}},etag={{.}}{{end}}
type Interface interface {
{{ $expandable := .Expand}}
{{ $packageAlias := .PackageAlias}}
{{ $resource := .Resource}}
{{range $index,$element := .Verbs}}
{{if strequal $element "Get"}}
{{ if $expandable }}utils.GetWithExpandFunc[{{tolower $packageAlias}}.{{$resource}}]{{ else }}utils.GetFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{- end -}}
{{if or (strequal $element "ListByRG") (strequal $element "List") }}utils.ListFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{if strequal $element "CreateOrUpdate"}}utils.CreateOrUpdateFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{if strequal $element "Delete"}}utils.DeleteFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{- end -}}
}
`
	TypeSubResourceTemplate = `
// +azure:client:verbs={{join .Verbs ";"}},resource={{.Resource}},subResource={{.SubResource}},packageName={{.Package}},packageAlias={{tolower .PackageAlias}},clientName={{.ClientName}},expand={{.Expand}}{{with .RateLimitKey}},rateLimitKey={{.}}{{end}}
type Interface interface {
{{ $expandable := .Expand}}
{{ $packageAlias := .PackageAlias}}
{{ $resource := .SubResource}}
{{range $index,$element := .Verbs}}
{{if strequal $element "Get"}}
{{ if $expandable }}utils.SubResourceGetWithExpandFunc[{{tolower $packageAlias}}.{{$resource}}]{{ else }}utils.SubResourceGetFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{- end -}}
{{if or (strequal $element "ListByRG") (strequal $element "List") }}utils.SubResourceListFunc[{{tolower $packageAlias}}.{{$resource}}]
{{- end -}}
{{if strequal $element "CreateOrUpdate"}}utils.SubResourceCreateOrUpdateFunc[{{tolower $packageAlias}}.{{$resource}}]
{{- end -}}
{{if strequal $element "Delete"}}utils.SubResourceDeleteFunc[{{tolower $packageAlias}}.{{$resource}}]{{- end -}}
{{- end -}}
}
`
	typesTemplateHelpers = template.FuncMap{
		"tolower":  strings.ToLower,
		"now":      time.Now,
		"join":     strings.Join,
		"strequal": strings.EqualFold,
	}
	typesSubResourceTemplate = template.Must(template.New("object-scaffolding-subresource").Funcs(typesTemplateHelpers).Parse(TypeTemplateHeader + TypeSubResourceTemplate))

	typesResourceTemplate = template.Must(template.New("object-scaffolding").Funcs(typesTemplateHelpers).Parse(TypeTemplateHeader + TypeResourceTemplate))
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
			if len(scaffoldOptions.SubResource) > 0 {
				if err := typesSubResourceTemplate.Execute(content, scaffoldOptions); err != nil {
					fmt.Printf("failed to generate client from template %s\n", err.Error())
					return
				}
			} else {
				if err := typesResourceTemplate.Execute(content, scaffoldOptions); err != nil {
					fmt.Printf("failed to generate client from template %s\n", err.Error())
					return
				}
			}
			formattedContent, err := format.Source(content.Bytes())
			if err != nil {
				fmt.Printf("failed to generate client %s\noriginal content: \n%s\n", err.Error(), content.String())
				return
			}
			var resourceName string
			if len(scaffoldOptions.SubResource) > 0 {
				resourceName = scaffoldOptions.SubResource
			} else {
				resourceName = scaffoldOptions.Resource
			}
			fileName := strings.ToLower(resourceName) + "client"
			if err := os.MkdirAll(fileName, 0755); err != nil && !os.IsExist(err) {
				fmt.Printf("failed to create dir %s\n", err.Error())
				return
			}
			if _, err = os.Lstat(fileName + "/interface.go"); err == nil {
				fmt.Printf("interface file %s already exists, skip\n", fileName+"/interface.go")
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
	rootCmd.Flags().StringVar(&scaffoldOptions.SubResource, "subresource", "", "subresource name")
	rootCmd.Flags().StringVar(&scaffoldOptions.RateLimitKey, "ratelimitkey", "", "ratelimit config key")
	rootCmd.Flags().BoolVar(&scaffoldOptions.CrossSubFactory, "cross-sub-factory-support", false, "cross sub factory support")
	rootCmd.Flags().BoolVar(&scaffoldOptions.Etag, "etag", false, "support etag")

	err := rootCmd.Execute()
	if err != nil {
		fmt.Printf("failed to generate interface %s\n", err.Error())
		os.Exit(1)
	}
}
