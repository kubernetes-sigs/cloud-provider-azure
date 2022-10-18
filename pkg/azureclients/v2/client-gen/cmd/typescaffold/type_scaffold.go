/*
Copyright 2022 The Kubernetes Authors.

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
	"io/fs"
	"io/ioutil"
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
	APIVersion   string
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
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/v2/utils"
)

// +azure:client:verbs=get;createorupdate;delete;list,resource={{.Resource}},packageName={{.Package}},packageAlias={{tolower .PackageAlias}},clientName={{.ClientName}},apiVersion="{{.APIVersion}}"
type Interface interface {
	utils.GetFunc[{{tolower .PackageAlias}}.{{.Resource}}]
	utils.ListFunc[{{tolower .PackageAlias}}.{{.Resource}}]
	utils.CreateOrUpdateFunc[{{tolower .PackageAlias}}.{{.Resource}}]
	utils.DeleteFunc[{{tolower .PackageAlias}}.{{.Resource}}]
}
`
	typesTemplateHelpers = template.FuncMap{
		"tolower": strings.ToLower,
		"now":     time.Now,
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
			typesTemplate.Execute(content, scaffoldOptions)
			formattedContent, err := format.Source(content.Bytes())
			if err != nil {
				fmt.Printf("failed to generate client %s\n", err.Error())
				return
			}
			fileName := strings.ToLower(scaffoldOptions.Resource) + "client"
			if err := os.MkdirAll(fileName, fs.ModeDir); err != nil {
				fmt.Printf("failed to create dir %s\n", err.Error())
				return
			}
			ioutil.WriteFile(fileName+"/interface.go", formattedContent, 0644)
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
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "resource"); err != nil {
		fmt.Printf("failed to mark resource option as required, %s\n", err)
		return
	}
	rootCmd.Flags().StringVar(&scaffoldOptions.APIVersion, "apiversion", "", "api version")
	if err := cobra.MarkFlagRequired(rootCmd.Flags(), "apiversion"); err != nil {
		fmt.Printf("failed to mark apiversion option as required, %s\n", err)
		return
	}
}
