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
	"html/template"
	"os"
	"os/exec"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

type ClientEntryConfig struct {
	ClientGenConfig
	PkgAlias          string
	PkgPath           string
	InterfaceTypeName string
}

type ClientFactoryGenerator struct {
	HeaderFile string `marker:",optional"`
}

func (ClientFactoryGenerator) RegisterMarkers(into *markers.Registry) error {
	return markers.RegisterAll(into, clientGenMarker, enableClientGenMarker)
}

func (generator ClientFactoryGenerator) Generate(ctx *genall.GenerationContext) error {
	clientRegistry := make(map[string]*ClientEntryConfig)
	importList := make(map[string]map[string]struct{})
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
			markerConf := marker.(ClientGenConfig)
			if !markerConf.OutOfSubscriptionScope {
				if _, ok := importList[root.PkgPath]; !ok {
					importList[root.PkgPath] = make(map[string]struct{})
				}

				clientRegistry[root.Name+typeInfo.Name] = &ClientEntryConfig{
					ClientGenConfig:   markerConf,
					PkgAlias:          root.Name,
					PkgPath:           root.PkgPath,
					InterfaceTypeName: typeInfo.Name,
				}
			}
		})
	}

	{
		file, err := ctx.OutputRule.Open(nil, "factory.go")
		if err != nil {
			return err
		}

		err = DumpHeaderToWriter(ctx, file, generator.HeaderFile, importList, "azclient")
		if err != nil {
			file.Close()
			return err
		}
		if err := AbstractClientFactoryInterfaceTemplate.Execute(file, clientRegistry); err != nil {
			file.Close()
			return err
		}
		file.Close()
	}
	{
		file, err := ctx.OutputRule.Open(nil, "factory_gen.go")
		if err != nil {
			return err
		}
		defer file.Close()

		codeimportList := make(map[string]map[string]struct{})
		for k, v := range importList {
			codeimportList[k] = v
		}
		for _, v := range clientRegistry {
			if v.ClientGenConfig.CrossSubFactory {
				codeimportList["sync"] = make(map[string]struct{})
				codeimportList["strings"] = make(map[string]struct{})
			}
			if v.AzureStackCloudAPIVersion != "" {
				importList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"] = make(map[string]struct{})
				importList["strings"] = make(map[string]struct{})
			}
		}

		codeimportList["github.com/Azure/azure-sdk-for-go/sdk/azcore"] = make(map[string]struct{})
		codeimportList["github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"] = make(map[string]struct{})
		codeimportList["sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"] = make(map[string]struct{})
		codeimportList["github.com/Azure/azure-sdk-for-go/sdk/azidentity"] = make(map[string]struct{})

		err = DumpHeaderToWriter(ctx, file, generator.HeaderFile, codeimportList, "azclient")
		if err != nil {
			return err
		}

		if err := AbstractClientFactoryImplTemplate.Execute(file, clientRegistry); err != nil {
			return err
		}

	}
	{
		file, err := ctx.OutputRule.Open(nil, "factory_test.go")
		if err != nil {
			return err
		}
		defer file.Close()
		testimportList := make(map[string]map[string]struct{})
		for k, v := range importList {
			testimportList[k] = v
		}

		testimportList["github.com/onsi/ginkgo/v2"] = map[string]struct{}{}
		testimportList["github.com/onsi/gomega"] = map[string]struct{}{}

		err = DumpHeaderToWriter(ctx, file, generator.HeaderFile, testimportList, "azclient")
		if err != nil {
			return err
		}

		if err := FactoryTestCaseTemplate.Execute(file, clientRegistry); err != nil {
			return err
		}
	}
	{
		mockFile, err := ctx.OutputRule.Open(nil, "mock_azclient/interface.go")
		if err != nil {
			return err
		}
		defer mockFile.Close()
		//nolint:gosec // G204 ignore this!
		cmd := exec.Command("mockgen", "-package", "mock_azclient", "-source", "factory.go", "-copyright_file", "../../hack/boilerplate/boilerplate.generatego.txt")
		cmd.Stdout = mockFile
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			return err
		}
	}
	return nil
}

func (ClientFactoryGenerator) CheckFilter() loader.NodeFilter {
	return func(node ast.Node) bool {
		// ignore structs
		_, isIface := node.(*ast.InterfaceType)
		return isIface
	}
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
	{{if $client.AzureStackCloudAPIVersion}}
	if factory.armConfig != nil && strings.EqualFold(factory.armConfig.Cloud, utils.AzureStackCloudName) {
		options.ClientOptions.APIVersion = {{.PkgAlias}}.AzureStackCloudAPIVersion
	}
	{{- end }}
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
