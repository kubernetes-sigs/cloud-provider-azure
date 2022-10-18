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

package generator

import "html/template"

type ClientGenConfig struct {
	Verbs        []string
	Resource     string
	PackageName  string
	PackageAlias string
	ClientName   string
	APIVersion   string `marker:"apiVersion,optional"`
	Expand       bool   `marker:"expand,optional"`
}

var ClientTemplate = template.Must(template.New("object-scaffolding-client-struct").Parse(`
type Client struct{
	*{{.PackageAlias}}.{{.ClientName}}
}
`))

var ClientFactoryTemplate = template.Must(template.New("object-scaffolding-factory").Parse(`
func New(subscriptionID string, credential azcore.TokenCredential, options *arm.ClientOptions) (Interface, error) {
	if options == nil {
		options = utils.GetDefaultOption("{{.APIVersion}}")
	}

	client, err := {{.PackageAlias}}.New{{.ClientName}}(subscriptionID, credential, options)
	if err != nil {
		return nil, err
	}
	return &Client{client}, nil
}
`))

const CreateOrUpdateFuncTemplateRaw = `
// CreateOrUpdate creates or updates a {{.Resource}}.
func (client *Client) CreateOrUpdate(ctx context.Context, resourceGroupName string, resourceName string, resource {{.PackageAlias}}.{{.Resource}}) (*{{.PackageAlias}}.{{.Resource}}, error) {
	resp, err := utils.NewPollerWrapper(client.{{.ClientName}}.BeginCreateOrUpdate(ctx, resourceGroupName, resourceName, resource, nil)).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	return &resp.{{.Resource}}, nil
}
`

var CreateOrUpdateFuncTemplate = template.Must(template.New("object-scaffolding-create-func").Parse(CreateOrUpdateFuncTemplateRaw))

const ListByRGFuncTemplateRaw = `
// List gets a list of {{.Resource}} in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string) (result []*{{.PackageAlias}}.{{.Resource}}, rerr error) {
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
`

var ListByRGFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Parse(ListByRGFuncTemplateRaw))

const ListFuncTemplateRaw = `
// List gets a list of {{.Resource}} in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string) (result []*{{.PackageAlias}}.{{.Resource}}, rerr error) {
	pager := client.{{.ClientName}}.NewListPager(resourceGroupName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
`

var ListFuncTemplate = template.Must(template.New("object-scaffolding-list-func").Parse(ListFuncTemplateRaw))

const DeleteFuncTemplateRaw = `
// Delete deletes a {{.Resource}} by name.
func (client *Client) Delete(ctx context.Context, resourceGroupName string, resourceName string) error {
	_, err := utils.NewPollerWrapper(client.BeginDelete(ctx, resourceGroupName, resourceName, nil)).WaitforPollerResp(ctx)
	return err
}
`

var DeleteFuncTemplate = template.Must(template.New("object-scaffolding-delete-func").Parse(DeleteFuncTemplateRaw))

const GetFuncTemplateRaw = `
// Get gets the {{.Resource}}
func (client *Client) Get(ctx context.Context, resourceGroupName string, resourceName string{{if .Expand}}, expand *string{{end}}) (result *{{.PackageAlias}}.{{.Resource}}, rerr error) {
	var ops *{{.PackageAlias}}.{{.ClientName}}GetOptions
	{{if .Expand}}if expand != nil {
		ops = &{{.PackageAlias}}.{{.ClientName}}GetOptions{ Expand: expand }
	}{{end}}

	resp, err := client.{{.ClientName}}.Get(ctx, resourceGroupName, resourceName, ops)
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.{{.Resource}}, nil
}
`

var GetFuncTemplate = template.Must(template.New("object-scaffolding-get-func").Parse(GetFuncTemplateRaw))

var ImportTemplate = template.Must(template.New("import").Parse(`{{.Alias}} "{{.Package}}"
`))

type ImportStatement struct {
	Alias   string
	Package string
}
