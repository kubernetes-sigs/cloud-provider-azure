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
	"html/template"
	"os"
	"os/exec"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
)

type ClientEntryConfig struct {
	ClientGenConfig
	PkgAlias          string
	PkgPath           string
	InterfaceTypeName string
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
		ClientGenConfig:   markerConf,
		PkgAlias:          root.Name,
		PkgPath:           root.PkgPath,
		InterfaceTypeName: typeName,
	}
	return nil
}

func (generator *ClientFactoryGenerator) Generate(_ *genall.GenerationContext) error {
	{
		var outContent bytes.Buffer
		if err := AbstractClientFactoryInterfaceTemplate.Execute(&outContent, generator.clientRegistry); err != nil {
			return err
		}
		file, err := os.Create("factory.go")
		if err != nil {
			return err
		}
		defer file.Close()
		err = DumpToWriter(file, generator.headerText, generator.importList, "azclient", &outContent)
		if err != nil {
			return err
		}
		fmt.Println("Generated client factory interface")
	}
	{
		var outContent bytes.Buffer
		if err := AbstractClientFactoryImplTemplate.Execute(&outContent, generator.clientRegistry); err != nil {
			return err
		}
		file, err := os.Create("factory_gen.go")
		if err != nil {
			return err
		}
		defer file.Close()
		importList := make(map[string]map[string]struct{})
		for k, v := range generator.importList {
			importList[k] = v
		}
		for _, v := range generator.clientRegistry {
			if v.ClientGenConfig.CrossSubFactory {
				importList["sync"] = map[string]struct{}{}
				importList["strings"] = map[string]struct{}{}
				break
			}
		}

		importList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
		importList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
		importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"] = make(map[string]struct{})
		importList["github.com/Azure/azure-sdk-for-go/sdk/azidentity"] = make(map[string]struct{})

		err = DumpToWriter(file, generator.headerText, importList, "azclient", &outContent)
		if err != nil {
			return err
		}
		fmt.Println("Generated client factory impl")
	}
	{
		var mockCache bytes.Buffer
		//nolint:gosec // G204 ignore this!
		cmd := exec.Command("mockgen", "-package", "mock_azclient", "sigs.k8s.io/cloud-provider-azure/pkg/azclient", "ClientFactory")
		cmd.Stdout = &mockCache
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			return err
		}
		if err := os.MkdirAll("mock_azclient", 0755); err != nil {
			return err
		}
		mockFile, err := os.Create("mock_azclient/interface.go")
		if err != nil {
			return err
		}
		defer mockFile.Close()
		err = DumpToWriter(mockFile, generator.headerText, nil, "", &mockCache)
		if err != nil {
			return err
		}
		fmt.Println("Generated client factory mock")
	}
	{
		var outContent bytes.Buffer
		if err := FactoryTestCaseTemplate.Execute(&outContent, generator.clientRegistry); err != nil {
			return err
		}
		file, err := os.Create("factory_test.go")
		if err != nil {
			return err
		}
		defer file.Close()
		importList := make(map[string]map[string]struct{})
		importList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{}
		importList["github.com/onsi/gomega"] = map[string]struct{}{}

		err = DumpToWriter(file, generator.headerText, importList, "azclient", &outContent)
		if err != nil {
			return err
		}
		fmt.Println("Generated client factory test")

	}
	return nil
}

var AbstractClientFactoryImplTemplate = template.Must(template.New("object-factory-impl").Parse(
	`
type ClientFactoryImpl struct {
	armConfig     *ARMClientConfig
	facotryConfig *ClientFactoryConfig
	cred               azcore.TokenCredential
	clientOptionsMutFn []func(option *arm.ClientOptions)
	{{range $key, $client := . -}}
	{{ if $client.CrossSubFactory -}}
	{{ $key }} sync.Map
	{{ else -}}
	{{ $key }} {{.PkgAlias}}.{{.InterfaceTypeName}} 
	{{end -}}
	{{end -}}
}

func NewClientFactory(config *ClientFactoryConfig, armConfig *ARMClientConfig, cred azcore.TokenCredential, clientOptionsMutFn ...func(option *arm.ClientOptions)) (ClientFactory,error) {
	if config == nil {
		config = &ClientFactoryConfig{}
	}
	if cred == nil {
		cred = &azidentity.DefaultAzureCredential{}
	}

	var err error 

	factory := &ClientFactoryImpl{
		armConfig: 	   armConfig,
		facotryConfig: config,
		cred:          cred,
		clientOptionsMutFn: clientOptionsMutFn,
	}
	{{range $key, $client := . }}
	{{- $resource := .Resource}}
	{{- if (gt (len .SubResource) 0) }}
	{{- $resource = .SubResource}}
	{{- end }}
	//initialize {{.PkgAlias}}
	{{ if $client.CrossSubFactory -}}
	_, err = factory.Get{{$resource}}ClientForSub(config.SubscriptionID)
	{{ else -}}
	factory.{{$key}}, err = factory.create{{$resource}}Client(config.SubscriptionID)
	{{ end -}}
	if err != nil {
		return nil, err
	}
	{{end -}}
	return factory, nil
}

{{range $key, $client := . }}
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
func (factory *ClientFactoryImpl) create{{$resource}}Client(subscription string)({{.PkgAlias}}.{{.InterfaceTypeName}},error) {
	//initialize {{.PkgAlias}}
	options, err := GetDefaultResourceClientOption(factory.armConfig, factory.facotryConfig)
	if err != nil {
		return nil, err
	}
	{{with $client.RateLimitKey}}
	//add ratelimit policy
	ratelimitOption := factory.facotryConfig.GetRateLimitConfig("{{.}}")
	rateLimitPolicy := ratelimit.NewRateLimitPolicy(ratelimitOption)
	if rateLimitPolicy != nil {
		options.ClientOptions.PerCallPolicies = append(options.ClientOptions.PerCallPolicies, rateLimitPolicy)
	}
	{{- end }}
	for _, optionMutFn := range factory.clientOptionsMutFn {
		if optionMutFn != nil {
			optionMutFn(options)
		}
	}
	return {{.PkgAlias}}.New(subscription, factory.cred, options)
}
{{ if $client.CrossSubFactory }}
func (factory *ClientFactoryImpl) Get{{$resource}}Client(){{.PkgAlias}}.{{.InterfaceTypeName}} {
	clientImp,_:= factory.{{ $key }}.Load(strings.ToLower(factory.facotryConfig.SubscriptionID))
	return clientImp.({{.PkgAlias}}.{{.InterfaceTypeName}})
}
func (factory *ClientFactoryImpl) Get{{$resource}}ClientForSub(subscriptionID string)({{.PkgAlias}}.{{.InterfaceTypeName}},error) {
	if subscriptionID == "" {
		subscriptionID = factory.facotryConfig.SubscriptionID
	}
	clientImp,loaded:= factory.{{ $key }}.Load(strings.ToLower(subscriptionID))
	if loaded {
		return clientImp.({{.PkgAlias}}.{{.InterfaceTypeName}}), nil
	}
	//It's not thread safe, but it's ok for now. because it will be called once. 
	clientImp, err := factory.create{{$resource}}Client(subscriptionID)
	if err != nil {
		return nil, err
	}
	factory.{{ $key }}.Store(strings.ToLower(subscriptionID), clientImp)
	return clientImp.({{.PkgAlias}}.{{.InterfaceTypeName}}), nil
}
{{- else }}
func (factory *ClientFactoryImpl) Get{{$resource}}Client(){{.PkgAlias}}.{{.InterfaceTypeName}} {
	return factory.{{ $key }}
}
{{ end }}
{{ end }}
`))

var AbstractClientFactoryInterfaceTemplate = template.Must(template.New("object-factory-impl").Parse(
	`
type ClientFactory interface {
	{{- range $key, $client := . }}
	{{$resource := $client.Resource }}
	{{- if (gt (len $client.SubResource) 0) }}
	{{- $resource = $client.SubResource -}}
	{{- end -}}
	Get{{$resource}}Client(){{.PkgAlias}}.{{.InterfaceTypeName}}
	{{- if .CrossSubFactory }}
	Get{{$resource}}ClientForSub(subscriptionID string)({{.PkgAlias}}.{{.InterfaceTypeName}},error)
	{{- end }}
	{{- end }}
}
`))

var FactoryTestCaseTemplate = template.Must(template.New("factory-test-case").Parse(
	`
	var _ = ginkgo.Describe("Factory", func() {
ginkgo.When("config is nil", func() {
			{{- range $key, $client := . }}
			{{$resource := $client.Resource }}
			{{- if (gt (len $client.SubResource) 0) }}
			{{- $resource = $client.SubResource -}}
			{{- end -}}
ginkgo.It("should create factory instance without painc - {{$resource}}", func() {
				factory, err := NewClientFactory(nil, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(factory).NotTo(gomega.BeNil())
				client := factory.Get{{$resource}}Client()
				gomega.Expect(client).NotTo(gomega.BeNil())
			})
			{{- end }}
		})
	})
`))
