package status

import (
	"context"
	"encoding/json"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/resources"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/google/uuid"
	"time"
)

const (
	defaultTimeoutSeconds = 10
	fullMetadata          = aztables.MetadataFormatFull
)

// TableClient allows reading and writing to azure tables.
type TableClient struct {
	tableURI resources.URI
	client   *aztables.Client
}

// NewTableClient Creates an azure table client.
func NewTableClient(client policy.Transporter, uri resources.URI) (*TableClient, error) {
	tableClient, err := aztables.NewClientWithNoCredential(uri.String(), &aztables.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: client,
		},
	})
	if err != nil {
		return nil, err
	}

	return &TableClient{
		tableURI: uri,
		client:   tableClient,
	}, nil
}

// Read reads a table record containing ingestion status.
func (c *TableClient) Read(ctx context.Context, ingestionSourceID string) (map[string]interface{}, error) {
	var emptyID = uuid.Nil.String()
	entity, err := c.client.GetEntity(ctx, ingestionSourceID, emptyID, nil)
	if err != nil {
		return nil, err
	}

	bytes := entity.Value
	m := make(map[string]interface{})
	err = json.Unmarshal(bytes, &m)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Write reads a table record containing ingestion status.
func (c *TableClient) Write(ctx context.Context, ingestionSourceID string, data map[string]interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeoutSeconds*time.Second)
	defer cancel()

	data["PartitionKey"] = ingestionSourceID
	data["RowKey"] = uuid.Nil.String()

	bytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	format := fullMetadata

	_, err = c.client.AddEntity(ctx, bytes, &aztables.AddEntityOptions{
		Format: &format,
	})

	return err
}
