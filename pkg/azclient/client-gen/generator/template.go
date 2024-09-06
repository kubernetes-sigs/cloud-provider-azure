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

package generator

import "html/template"

type ClientGenConfig struct {
	Verbs           []string `marker:",optional"`
	Resource        string
	SubResource     string `marker:"subResource,optional"`
	PackageName     string
	PackageAlias    string
	ClientName      string
	Expand          bool   `marker:"expand,optional"`
	RateLimitKey    string `marker:"rateLimitKey,optional"`
	CrossSubFactory bool   `marker:"crossSubFactory,optional"`
}

var ClientTemplate = template.Must(template.New("object-scaffolding-client-struct").Parse(`
type Client struct{
	*{{.PackageAlias}}.{{.ClientName}}
	subscriptionID string
	tracer tracing.Tracer
}
`))

var ClientFactoryTemplate = template.Must(template.New("object-scaffolding-factory").Parse(`
func New(subscriptionID string, credential azcore.TokenCredential, options *arm.ClientOptions) (Interface, error) {
	if options == nil {
		options = utils.GetDefaultOption()
	}
	tr := options.TracingProvider.NewTracer(utils.ModuleName, utils.ModuleVersion)

	client, err := {{.PackageAlias}}.New{{.ClientName}}(subscriptionID, credential, options)
	if err != nil {
		return nil, err
	}
	return &Client{ 
		{{.ClientName}}: client, 
		subscriptionID:  subscriptionID,
		tracer:          tr, 
	}, nil
}
`))

var CreateOrUpdateFuncTemplate = template.Must(template.New("object-scaffolding-create-func").Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const CreateOrUpdateOperationName = "{{.ClientName}}.Create"
// CreateOrUpdate creates or updates a {{$resource}}.
func (client *Client) CreateOrUpdate(ctx context.Context, resourceGroupName string, resourceName string,{{with .SubResource}}parentResourceName string, {{end}} resource {{.PackageAlias}}.{{$resource}}) (result *{{.PackageAlias}}.{{$resource}}, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "create_or_update")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, CreateOrUpdateOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := utils.NewPollerWrapper(client.{{.ClientName}}.BeginCreateOrUpdate(ctx, resourceGroupName, resourceName,{{with .SubResource}}parentResourceName,{{end}} resource, nil)).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return &resp.{{$resource}}, nil
	}
	return nil, nil
}
`))

var ListByRGFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const ListOperationName = "{{.ClientName}}.List"
// List gets a list of {{$resource}} in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string{{with .SubResource}}, parentResourceName string{{end}}) (result []*{{.PackageAlias}}.{{$resource}}, err error) {
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

var ListFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const ListOperationName = "{{.ClientName}}.List"
// List gets a list of {{$resource}} in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string{{with .SubResource}}, parentResourceName string{{end}}) (result []*{{.PackageAlias}}.{{$resource}}, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "list")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, ListOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.{{.ClientName}}.NewListPager(resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} nil)
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

var DeleteFuncTemplate = template.Must(template.New("object-scaffolding-delete-func").Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const DeleteOperationName = "{{.ClientName}}.Delete"
// Delete deletes a {{$resource}} by name.
func (client *Client) Delete(ctx context.Context, resourceGroupName string, {{with .SubResource}} parentResourceName string, {{end}}resourceName string) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "delete")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, DeleteOperationName, client.tracer, nil)
	defer endSpan(err)
	_, err = utils.NewPollerWrapper(client.BeginDelete(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName, nil)).WaitforPollerResp(ctx)
	return err
}
`))

var GetFuncTemplate = template.Must(template.New("object-scaffolding-get-func").Parse(`
{{- $resource := .Resource}}
{{- if (gt (len .SubResource) 0) }}
{{- $resource = .SubResource}}
{{- end }}
const GetOperationName = "{{.ClientName}}.Get"
// Get gets the {{$resource}}
func (client *Client) Get(ctx context.Context, resourceGroupName string, {{with .SubResource}}parentResourceName string,{{end}} resourceName string{{if .Expand}}, expand *string{{end}}) (result *{{.PackageAlias}}.{{$resource}}, err error) {
	{{ if .Expand}}var ops *{{.PackageAlias}}.{{.ClientName}}GetOptions
	if expand != nil {
		ops = &{{.PackageAlias}}.{{.ClientName}}GetOptions{ Expand: expand }
	}{{- end}}
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "{{ $resource }}", "get")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := client.{{.ClientName}}.Get(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName,{{if .Expand}}ops{{else}}nil{{end}} )
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.{{$resource}}, nil
}
`))

var ImportTemplate = template.Must(template.New("import").Parse(
	`
import (
	{{ range $package, $Entry := . }}
	{{- range $alias, $flag := $Entry}}{{$alias}} {{end}}"{{$package}}"
	{{ end -}}
)
`))

type ImportStatement struct {
	Alias   string
	Package string
}

var TestSuiteTemplate = template.Must(template.New("object-scaffolding-test-suite").Parse(
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
{{if .SubResource}}var parentResourceName = "testParentResource"{{- end }}
var subscriptionID string
var location = "eastus"
var resourceGroupClient *armresources.ResourceGroupsClient
var err error
var recorder *recording.Recorder
var realClient Interface

var _ = ginkgo.BeforeSuite(func(ctx context.Context) {
	recorder, err = recording.NewRecorder("testdata/{{$resource}}")
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	subscriptionID = recorder.SubscriptionID()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	cred := recorder.TokenCredential()
	resourceGroupClient, err = armresources.NewResourceGroupsClient(subscriptionID, cred, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: recorder.HTTPClient(),
			TracingProvider: utils.TracingProvider,
		},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	realClient, err = New(subscriptionID, recorder.TokenCredential(), &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: recorder.HTTPClient(),
			TracingProvider: utils.TracingProvider,
		},
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

var TestCaseTemplate = template.Must(template.New("object-scaffolding-test-case").Parse(
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
			newResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName, *newResource)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
			gomega.Expect(strings.EqualFold(*newResource.Name,resourceName)).To(gomega.BeTrue())
		})
	})
{{end -}}
{{if $HasGet}}
ginkgo.When("get requests are raised", func() {
ginkgo.It("should not return error", func(ctx context.Context) {
			newResource, err := realClient.Get(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName{{if .Expand}}, nil{{end}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
		})
	})
ginkgo.When("invalid get requests are raised", func() {
ginkgo.It("should return 404 error", func(ctx context.Context) {
			newResource, err := realClient.Get(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName+"notfound"{{if .Expand}}, nil{{end}})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(newResource).To(gomega.BeNil())
		})
	})
{{end -}}
{{if $HasCreateOrUpdate}}
ginkgo.When("update requests are raised", func() {
ginkgo.It("should not return error", func(ctx context.Context) {
			newResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName, *newResource)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(newResource).NotTo(gomega.BeNil())
		})
	})
{{end -}}
{{if or $HasListByRG $HasList}}
ginkgo.When("list requests are raised", func() {
ginkgo.It("should not return error", func(ctx context.Context) {
			resourceList, err := realClient.List(ctx, resourceGroupName,{{with .SubResource}}parentResourceName{{end}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resourceList).NotTo(gomega.BeNil())
			gomega.Expect(len(resourceList)).To(gomega.Equal(1))
			gomega.Expect(*resourceList[0].Name).To(gomega.Equal(resourceName))
		})
	})
ginkgo.When("invalid list requests are raised", func() {
ginkgo.It("should return error", func(ctx context.Context) {
			resourceList, err := realClient.List(ctx, resourceGroupName+"notfound",{{with .SubResource}}parentResourceName{{end}})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(resourceList).To(gomega.BeNil())
		})
	})
{{end -}}
{{if $HasDelete}}
ginkgo.When("deletion requests are raised", func() {
ginkgo.It("should not return error", func(ctx context.Context) {
			err = realClient.Delete(ctx, resourceGroupName,{{with .SubResource}}parentResourceName,{{end}} resourceName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
{{end }}
	if afterAllFunc != nil {
		ginkgo.AfterAll(afterAllFunc)
	}
})
`))

var TestCaseCustomTemplate = template.Must(template.New("object-scaffolding-test-case-custom").Parse(
	`
	{{ $resource := .Resource}}
	{{ if (gt (len .SubResource) 0) }}
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
func init() {
	additionalTestCases = func() {
	}

	beforeAllFunc = func(ctx context.Context) {
		{{if not $HasCreateOrUpdate}} 
		newResource = &{{.PackageAlias}}.{{$resource}}{
			{{if not .SubResource}}Location: to.Ptr(location), {{- end -}}
		}
		{{- end -}}
	}
	afterAllFunc = func(ctx context.Context) {
	}
}

`))
