package status

import (
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/google/uuid"
)

const (
	defaultTimeoutMsec = 10000
	fullMetadata       = "application/json;odata=fullmetadata"
)

// TableClient allows reading and writing to azure tables.
type TableClient struct {
	tableURI resources.URI
	client   storage.Client
	service  storage.TableServiceClient
	table    *storage.Table
}

// NewTableClient Creates an azure table client.
func NewTableClient(uri resources.URI) (*TableClient, error) {
	c, err := storage.NewAccountSASClientFromEndpointToken(uri.URL().String(), uri.SAS().Encode())
	if err != nil {
		return nil, err
	}

	ts := c.GetTableService()

	return &TableClient{
		tableURI: uri,
		client:   c,
		service:  ts,
		table:    ts.GetTableReference(uri.ObjectName()),
	}, nil
}

// Read reads a table record cotaining ingestion status.
func (c *TableClient) Read(ingestionSourceID string) (map[string]interface{}, error) {
	var emptyID = uuid.Nil.String()
	entity := c.table.GetEntityReference(ingestionSourceID, emptyID)

	err := entity.Get(defaultTimeoutMsec, fullMetadata, nil)
	if err != nil {
		return nil, err
	}

	return entity.Properties, nil
}

// Write reads a table record cotaining ingestion status.
func (c *TableClient) Write(ingestionSourceID string, data map[string]interface{}) error {
	var emptyID = uuid.Nil.String()
	entity := c.table.GetEntityReference(ingestionSourceID, emptyID)
	entity.Properties = data

	options := &storage.EntityOptions{}
	options.Timeout = defaultTimeoutMsec

	err := entity.Insert(fullMetadata, options)
	if err != nil {
		return err
	}

	return nil
}
