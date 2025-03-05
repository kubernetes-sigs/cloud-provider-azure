// /*
// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

package generator

import (
	"strings"
	"text/template"
)

var funcMap = template.FuncMap{
	"toLower": strings.ToLower,
}

type ClientGenConfig struct {
	Verbs                     []string `marker:",optional"`
	Resource                  string   `marker:",optional"`
	SubResource               string   `marker:"subResource,optional"`
	PackageName               string   `marker:",optional"`
	PackageAlias              string   `marker:",optional"`
	ClientName                string   `marker:",optional"`
	OutOfSubscriptionScope    bool     `marker:"outOfSubscriptionScope,optional"`
	Expand                    bool     `marker:"expand,optional"`
	RateLimitKey              string   `marker:"rateLimitKey,optional"`
	CrossSubFactory           bool     `marker:"crossSubFactory,optional"`
	Etag                      bool     `marker:"etag,optional"`
	AzureStackCloudAPIVersion string   `marker:"azureStackCloudAPIVersion,optional"`
	MooncakeApiVersion        string   `marker:"mooncakeApiVersion,optional"`
}

var ClientTemplate = template.Must(template.New("object-scaffolding-client-struct").Funcs(funcMap).Parse(`
{{- with .AzureStackCloudAPIVersion}}
const AzureStackCloudAPIVersion = "{{.}}"
{{- end }}
{{- with .MooncakeApiVersion}}
const MooncakeApiVersion = "{{.}}"
{{- end }}
type Client struct{
	*{{.PackageAlias}}.{{.ClientName}}
	{{if not .OutOfSubscriptionScope -}}subscriptionID string {{- end}}
	tracer tracing.Tracer
}
`))

var ClientFactoryTemplate = template.Must(template.New("object-scaffolding-factory").Funcs(funcMap).Parse(`
func New({{if not .OutOfSubscriptionScope}}subscriptionID string, {{end}}credential azcore.TokenCredential, options *arm.ClientOptions) (Interface, error) {
	if options == nil {
		options = utils.GetDefaultOption()
	}
	tr := options.TracingProvider.NewTracer(utils.ModuleName, utils.ModuleVersion)
	{{with .Etag}}
	options.ClientOptions.PerCallPolicies = append(options.ClientOptions.PerCallPolicies, utils.FuncPolicyWrapper(etag.AppendEtag))
	{{- end}}
	client, err := {{.PackageAlias}}.New{{.ClientName}}({{if not .OutOfSubscriptionScope}}subscriptionID,{{end}} credential, options)
	if err != nil {
		return nil, err
	}
	return &Client{ 
		{{.ClientName}}: client, 
		{{if not .OutOfSubscriptionScope}}subscriptionID: subscriptionID,{{end}}
		tracer:          tr, 
	}, nil
}
`))

var CreateOrUpdateFuncTemplate = template.Must(template.New("object-scaffolding-create-func").Funcs(funcMap).Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const CreateOrUpdateOperationName = "{{.ClientName}}.Create"
// CreateOrUpdate creates or updates a {{$resource}}.
func (client *Client) CreateOrUpdate(ctx context.Context, resourceGroupName string,{{if .SubResource}}{{toLower .Resource}}Name string, {{end}} {{toLower $resource}}Name string, resource {{.PackageAlias}}.{{$resource}}) (result *{{.PackageAlias}}.{{$resource}}, err error) {
	{{if .OutOfSubscriptionScope -}}
	metricsCtx := metrics.BeginARMRequestWithAttributes(attribute.String("resource", "{{ $resource }}"), attribute.String("method", "create_or_update"))
	{{else -}}
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "create_or_update")
	{{end -}}
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, CreateOrUpdateOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := utils.NewPollerWrapper(client.{{.ClientName}}.BeginCreateOrUpdate(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} {{toLower $resource}}Name, resource, nil)).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return &resp.{{$resource}}, nil
	}
	return nil, nil
}
`))

var ListByRGFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Funcs(funcMap).Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const ListOperationName = "{{.ClientName}}.List"
// List gets a list of {{$resource}} in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string{{if .SubResource}}, {{toLower .Resource}}Name string{{end}}) (result []*{{.PackageAlias}}.{{$resource}}, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "list")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, ListOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.{{.ClientName}}.NewListByResourceGroupPager(resourceGroupName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
`))

var ListFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Funcs(funcMap).Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const ListOperationName = "{{.ClientName}}.List"
// List gets a list of {{$resource}} in the resource group.
func (client *Client) List(ctx context.Context,{{if .OutOfSubscriptionScope}} scopeName{{else}} resourceGroupName{{end}} string{{if .SubResource}}, {{toLower .Resource}}Name string{{end}}) (result []*{{.PackageAlias}}.{{$resource}}, err error) {
	{{if .OutOfSubscriptionScope -}}
	metricsCtx := metrics.BeginARMRequestWithAttributes(attribute.String("resource", "{{ $resource }}"), attribute.String("method", "list"))
	{{else -}}
	metricsCtx := metrics.BeginARMRequest({{if .OutOfSubscriptionScope}}scopeName{{else}}client.subscriptionID, resourceGroupName,{{end}} "{{ $resource }}", "list")
	{{end -}}
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, ListOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.{{.ClientName}}.NewListPager({{if .OutOfSubscriptionScope}}scopeName{{else}}resourceGroupName{{end}},{{if .SubResource}} {{toLower .Resource}}Name,{{end}} nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
`))

var DeleteFuncTemplate = template.Must(template.New("object-scaffolding-delete-func").Funcs(funcMap).Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const DeleteOperationName = "{{.ClientName}}.Delete"
// Delete deletes a {{$resource}} by name.
func (client *Client) Delete(ctx context.Context, resourceGroupName string, {{if .SubResource}} {{toLower .Resource}}Name string, {{end}}{{toLower $resource}}Name string) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "delete")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, DeleteOperationName, client.tracer, nil)
	defer endSpan(err)
	_, err = utils.NewPollerWrapper(client.BeginDelete(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} {{toLower $resource}}Name, nil)).WaitforPollerResp(ctx)
	return err
}
`))

var GetFuncTemplate = template.Must(template.New("object-scaffolding-get-func").Funcs(funcMap).Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const GetOperationName = "{{.ClientName}}.Get"
// Get gets the {{$resource}}
func (client *Client) Get(ctx context.Context, resourceGroupName string, {{if .SubResource}}{{toLower .Resource}}Name string,{{end}} {{toLower $resource}}Name string{{if .Expand}}, expand *string{{end}}) (result *{{.PackageAlias}}.{{$resource}}, err error) {
	{{ if .Expand -}}var ops *{{.PackageAlias}}.{{.ClientName}}GetOptions
	if expand != nil {
		ops = &{{.PackageAlias}}.{{.ClientName}}GetOptions{ Expand: expand }
	}{{ end }}
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "get")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := client.{{.ClientName}}.Get(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} {{toLower $resource}}Name,{{if .Expand}} ops{{else}} nil{{end}})
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.{{$resource}}, nil
}
`))

var HeaderTemplate = template.Must(template.New("import").Funcs(funcMap).Parse(
	`

// Code generated by client-gen. DO NOT EDIT.
package {{.PackageName}}

import (
	{{ range $package, $Entry := .ImportList }}
	{{- range $alias, $flag := $Entry}}{{$alias}} {{end}}"{{$package}}"
	{{ end -}}
)
`))

type HeaderTemplateVariable struct {
	PackageName string
	ImportList  map[string]map[string]struct{}
}

var TestSuiteTemplate = template.Must(template.New("object-scaffolding-test-suite").Funcs(funcMap).Parse(
	`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
func TestClient(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Client Suite")
}

var resourceGroupName = "aks-cit-{{$resource}}"
var resourceName = "testResource"
{{if .SubResource}}var {{toLower .Resource}}Name = "testParentResource"{{- end }}
var subscriptionID string
var location string
var resourceGroupClient *armresources.ResourceGroupsClient
var err error
var recorder *recording.Recorder
var cloudConfig *cloud.Configuration
var realClient Interface
var clientOption azcore.ClientOptions 

var _ = ginkgo.BeforeSuite(func(ctx context.Context) {
	recorder, cloudConfig, location, err = recording.NewRecorder()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	subscriptionID = recorder.SubscriptionID()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	cred := recorder.TokenCredential()
	clientOption = azcore.ClientOptions{
		Transport:       recorder.HTTPClient(),
		TracingProvider: utils.TracingProvider,
		Telemetry: policy.TelemetryOptions{
			ApplicationID: "ccm-resource-group-client",
		},
		Cloud: *cloudConfig,
	}
	rgClientOption := clientOption
	rgClientOption.Telemetry.ApplicationID = "ccm-resource-group-client"
	resourceGroupClient, err = armresources.NewResourceGroupsClient(subscriptionID, cred, &arm.ClientOptions{
		ClientOptions: rgClientOption,
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	realClientOption := clientOption
	realClientOption.Telemetry.ApplicationID = "ccm-{{$resource}}-client"
{{- if .MooncakeApiVersion}}
	if location == "chinaeast2" {
		realClientOption.APIVersion = MooncakeApiVersion
	}
{{- end }}
	realClient, err = New({{if not .OutOfSubscriptionScope}}subscriptionID, {{end}}recorder.TokenCredential(), &arm.ClientOptions{
		ClientOptions: realClientOption,
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = resourceGroupClient.CreateOrUpdate(
		ctx,
		resourceGroupName,
		armresources.ResourceGroup{
			Location: to.Ptr(location),
		},
		nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
})

var _ = ginkgo.AfterSuite(func(ctx context.Context) {
	poller, err := resourceGroupClient.BeginDelete(ctx, resourceGroupName, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = poller.PollUntilDone(ctx, nil)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = recorder.Stop()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
})
`))

var TestCaseTemplate = template.Must(template.New("object-scaffolding-test-case").Funcs(funcMap).Parse(
	`
{{ $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{ $resource = .SubResource}}
{{- end -}}
{{- $HasCreateOrUpdate := false }}
{{-  $HasGet := false }}
{{-  $HasDelete := false }}
{{- $HasListByRG := false }}
{{- $HasList := false }}
{{- range .Verbs}}
{{- if eq . "createorupdate"}}{{$HasCreateOrUpdate = true}}{{end}}
{{- if eq . "get"}}{{$HasGet = true}}{{end}}
{{- if eq . "delete"}}{{$HasDelete = true}}{{end}}
{{- if eq . "listbyrg"}}{{$HasListByRG = true}}{{end}}
{{- if eq . "list"}}{{$HasList = true}}{{end}}
{{- end -}}
var beforeAllFunc func(context.Context)
var afterAllFunc func(context.Context)
var additionalTestCases func()
{{if or $HasCreateOrUpdate}}var newResource *{{.PackageAlias}}.{{$resource}} = &{{.PackageAlias}}.{{$resource}}{} {{- end }}

var _ = ginkgo.Describe("{{.ClientName}}",ginkgo.Ordered, func() {

	if beforeAllFunc != nil {
		ginkgo.BeforeAll(beforeAllFunc)
	}
	
	if additionalTestCases != nil {
		additionalTestCases()
	}

{{if $HasCreateOrUpdate}}
	ginkgo.When("creation requests are raised", func() {
		ginkgo.It("should not return error", func(ctx context.Context) {
			newResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName, *newResource)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
			gomega.Expect(strings.EqualFold(*newResource.Name,resourceName)).To(gomega.BeTrue())
		})
	})
{{end -}}
{{if $HasGet}}
	ginkgo.When("get requests are raised", func() {
		ginkgo.It("should not return error", func(ctx context.Context) {
			newResource, err := realClient.Get(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName{{if .Expand}}, nil{{end}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
		})
	})
	ginkgo.When("invalid get requests are raised", func() {
		ginkgo.It("should return 404 error", func(ctx context.Context) {
			newResource, err := realClient.Get(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName+"notfound"{{if .Expand}}, nil{{end}})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(newResource).To(gomega.BeNil())
		})
	})
{{end -}}
{{if $HasCreateOrUpdate}}
	ginkgo.When("update requests are raised", func() {
		ginkgo.It("should not return error", func(ctx context.Context) {
			newResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName, *newResource)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
		})
{{if .Etag}}
		ginkgo.It("should return error", func(ctx context.Context) {
			newResource.Etag = to.Ptr("invalid")
			_, err := realClient.CreateOrUpdate(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName, *newResource)
			gomega.Expect(err).To(gomega.HaveOccurred())
		})
{{end -}}
	})
{{end -}}
{{if or $HasListByRG $HasList}}
	ginkgo.When("list requests are raised", func() {
		ginkgo.It("should not return error", func(ctx context.Context) {
			resourceList, err := realClient.List(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name{{end}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resourceList).NotTo(gomega.BeNil())
			gomega.Expect(len(resourceList)).To(gomega.Equal(1))
			gomega.Expect(*resourceList[0].Name).To(gomega.Equal(resourceName))
		})
	})
	ginkgo.When("invalid list requests are raised", func() {
		ginkgo.It("should return error", func(ctx context.Context) {
			resourceList, err := realClient.List(ctx, resourceGroupName+"notfound",{{if .SubResource}}{{toLower .Resource}}Name{{end}})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(resourceList).To(gomega.BeNil())
		})
	})
{{end -}}
{{if $HasDelete}}
	ginkgo.When("deletion requests are raised", func() {
		ginkgo.It("should not return error", func(ctx context.Context) {
			err = realClient.Delete(ctx, resourceGroupName,{{if .SubResource}}{{toLower .Resource}}Name,{{end}} resourceName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
{{end }}
	if afterAllFunc != nil {
		ginkgo.AfterAll(afterAllFunc)
	}
})
`))
