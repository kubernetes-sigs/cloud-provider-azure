package ingest

import (
	"context"
	"io"
	"net/http"

	"github.com/Azure/azure-kusto-go/kusto"
)

type QueryClient interface {
	io.Closer
	Auth() kusto.Authorization
	Endpoint() string
	Query(ctx context.Context, db string, query kusto.Statement, options ...kusto.QueryOption) (*kusto.RowIterator, error)
	Mgmt(ctx context.Context, db string, query kusto.Statement, options ...kusto.MgmtOption) (*kusto.RowIterator, error)
	HttpClient() *http.Client
	ClientDetails() *kusto.ClientDetails
}
