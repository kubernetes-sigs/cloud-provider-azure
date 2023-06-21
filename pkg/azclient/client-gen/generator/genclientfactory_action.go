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
	"html/template"
	"os"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
)

type ClientEntryConfig struct {
	PkgAlias          string
	PkgPath           string
	InterfaceTypeName string
	RateLimitKey      string
}

type ClientFactoryGenerator struct {
	clientRegistry map[string]*ClientEntryConfig
	importList     map[string]map[string]struct{}
	headerText     string
}

func NewGenerator(headerText string) *ClientFactoryGenerator {
	return &ClientFactoryGenerator{
		clientRegistry: make(map[string]*ClientEntryConfig),
		importList:     make(map[string]map[string]struct{}),
		headerText:     headerText,
	}
}

func (generator *ClientFactoryGenerator) RegisterClient(_ *genall.GenerationContext, root *loader.Package, typeName string, markerConf ClientGenConfig, _ string) error {
	if _, ok := generator.importList[root.PkgPath]; !ok {
		generator.importList[root.PkgPath] = make(map[string]struct{})
	}

	generator.clientRegistry[root.Name+typeName] = &ClientEntryConfig{
		PkgAlias:          root.Name,
		PkgPath:           root.PkgPath,
		InterfaceTypeName: typeName,
		RateLimitKey:      markerConf.RateLimitKey,
	}
	return nil
}

func (generator *ClientFactoryGenerator) Generate(_ *genall.GenerationContext) error {
	var outContent bytes.Buffer
	if err := AbstractClientFactoryTemplate.Execute(&outContent, generator.clientRegistry); err != nil {
		return err
	}
	file, err := os.Create("factory_gen.go")
	if err != nil {
		return err
	}
	defer file.Close()
	generator.importList["strings"] = make(map[string]struct{})
	generator.importList["sync"] = make(map[string]struct{})
	generator.importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
	generator.importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"] = make(map[string]struct{})

	return DumpToWriter(file, generator.headerText, generator.importList, "azclient", &outContent)
}

var AbstractClientFactoryTemplate = template.Must(template.New("object-scaffolding-test-case-custom").Parse(
	`
type ClientFactory struct {
	*ClientFactoryConfig
	cred               azcore.TokenCredential
	{{ range $key, $client := . -}}
	{{ $key }}Registry sync.Map
	{{end -}}
}

func NewClientFactory(config *ClientFactoryConfig, cred azcore.TokenCredential) *ClientFactory {
	return &ClientFactory{
		ClientFactoryConfig: config,
		cred:                cred,
	}
}

{{range $key, $client := . }}
func (factory *ClientFactory) Get{{.PkgAlias}}{{.InterfaceTypeName}}(subscription string) ({{.PkgAlias}}.{{.InterfaceTypeName}}, error) {
	subID := strings.ToLower(subscription)

	options, err := GetDefaultResourceClientOption(factory.ClientFactoryConfig)
	if err != nil {
		return nil, err
	}
	{{with .RateLimitKey -}}
	//add ratelimit policy
	ratelimitOption := factory.ClientFactoryConfig.GetRateLimitConfig("{{.}}")
	rateLimitPolicy := ratelimit.NewRateLimitPolicy(ratelimitOption)
	options.ClientOptions.PerCallPolicies = append(options.ClientOptions.PerCallPolicies, rateLimitPolicy)
	{{- end }}
	defaultClient, err := {{.PkgAlias}}.New(subscription, factory.cred, options)
	if err != nil {
		return nil, err
	}
	client, _ := factory.{{ $key }}Registry.LoadOrStore(subID, &defaultClient)

	return *client.(*{{.PkgAlias}}.{{.InterfaceTypeName}}), nil
}
{{ end }}
`))
