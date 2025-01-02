package azkustoingest

import (
	"context"
	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/Azure/azure-kusto-go/azkustodata/query"
	"github.com/Azure/azure-kusto-go/azkustodata/query/v1"
	"github.com/Azure/azure-kusto-go/azkustodata/types"
	"net/http"
)

type mockClient struct {
	endpoint string
	auth     azkustodata.Authorization
	onMgmt   func(ctx context.Context, db string, query azkustodata.Statement, options ...azkustodata.QueryOption) (v1.Dataset, error)
}

func (m mockClient) Query(_ context.Context, _ string, _ azkustodata.Statement, _ ...azkustodata.QueryOption) (query.Dataset, error) {
	panic("not implemented")
}

func (m mockClient) IterativeQuery(_ context.Context, _ string, _ azkustodata.Statement, _ ...azkustodata.QueryOption) (query.IterativeDataset, error) {
	panic("not implemented")
}

func (m mockClient) ClientDetails() *azkustodata.ClientDetails {
	return azkustodata.NewClientDetails("test", "test")
}
func (m mockClient) HttpClient() *http.Client {
	return &http.Client{}
}

func (m mockClient) Close() error {
	return nil
}

func (m mockClient) Auth() azkustodata.Authorization {
	return m.auth
}

func (m mockClient) Endpoint() string {
	return m.endpoint
}

func (m mockClient) Mgmt(ctx context.Context, db string, query azkustodata.Statement, options ...azkustodata.QueryOption) (v1.Dataset, error) {
	if m.onMgmt != nil {
		rows, err := m.onMgmt(ctx, db, query, options...)
		if err != nil || rows != nil {
			return rows, err
		}
	}

	if query.String() == ".get kusto identity token" {
		return v1.NewDataset(ctx, errors.OpMgmt, v1.V1{
			Tables: []v1.RawTable{
				{
					TableName: "Table",
					Columns: []v1.RawColumn{
						{
							ColumnName: "AuthorizationContext",
							ColumnType: string(types.String),
						},
					},
					Rows: []v1.RawRow{
						{
							Row:    []interface{}{"mock"},
							Errors: nil,
						},
					},
				},
			}})
	}

	return v1.NewDataset(ctx, errors.OpMgmt, v1.V1{
		Tables: []v1.RawTable{
			{
				TableName: "Table",
				Columns: []v1.RawColumn{
					{
						ColumnName: "ResourceTypeName",
						ColumnType: string(types.String),
					},
					{
						ColumnName: "StorageRoot",
						ColumnType: string(types.String),
					},
				},
				Rows: []v1.RawRow{},
			},
		}})

}

func newMockClient() mockClient {
	return mockClient{
		endpoint: "localhost",
		auth: azkustodata.Authorization{
			TokenProvider: &azkustodata.TokenProvider{},
		},
	}
}
