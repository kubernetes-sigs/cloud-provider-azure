package azkustoingest

import (
	"context"
	"fmt"
	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/properties"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/queued"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/resources"
	"github.com/google/uuid"
	"io"
	"net/http"
)

type Ingestor interface {
	io.Closer
	FromFile(ctx context.Context, fPath string, options ...FileOption) (*Result, error)
	FromReader(ctx context.Context, reader io.Reader, options ...FileOption) (*Result, error)
}

// Ingestion provides data ingestion from external sources into Kusto.
type Ingestion struct {
	db    string
	table string

	client QueryClient
	mgr    *resources.Manager

	fs queued.Queued

	bufferSize int
	maxBuffers int

	withoutEndpointCorrection    bool
	customIngestConnectionString *azkustodata.ConnectionStringBuilder
	httpClient                   *http.Client
	applicationForTracing        string
	clientVersionForTracing      string
}

// New is a constructor for Ingestion.
func New(kcsb *azkustodata.ConnectionStringBuilder, options ...Option) (*Ingestion, error) {
	i := getOptions(options)

	if !i.withoutEndpointCorrection {
		newKcsb := *kcsb
		newKcsb.DataSource = addIngestPrefix(newKcsb.DataSource)
		kcsb = &newKcsb
	}
	clientDetails := azkustodata.NewClientDetails(kcsb.ApplicationForTracing, kcsb.UserForTracing)
	i.applicationForTracing = clientDetails.ApplicationForTracing()
	i.clientVersionForTracing = clientDetails.ClientVersionForTracing()

	var client *azkustodata.Client
	var err error

	if i.httpClient != nil {
		client, err = azkustodata.New(kcsb, azkustodata.WithHttpClient(i.httpClient))
	} else {
		client, err = azkustodata.New(kcsb)
	}

	if err != nil {
		return nil, err
	}

	return newFromClient(client, i)
}

func newFromClient(client QueryClient, i *Ingestion) (*Ingestion, error) {
	mgr, err := resources.New(client)
	if err != nil {
		client.Close()
		return nil, err
	}

	i.client = client
	i.mgr = mgr

	fs, err := queued.New(i.db, i.table, mgr, client.HttpClient(), i.applicationForTracing, i.clientVersionForTracing, queued.WithStaticBuffer(i.bufferSize, i.maxBuffers))
	if err != nil {
		mgr.Close()
		client.Close()
		return nil, err
	}

	i.fs = fs

	return i, nil
}

func (i *Ingestion) prepForIngestion(ctx context.Context, options []FileOption, props properties.All, source SourceScope) (*Result, properties.All, error) {
	result := newResult()

	auth, err := i.mgr.AuthContext(ctx)
	if err != nil {
		return nil, properties.All{}, err
	}

	props.Ingestion.Additional.AuthContext = auth

	for _, o := range options {
		if err := o.Run(&props, QueuedClient, source); err != nil {
			return nil, properties.All{}, err
		}
	}

	if source == FromReader && props.Ingestion.Additional.Format == DFUnknown {
		props.Ingestion.Additional.Format = CSV
	}

	if props.Ingestion.Additional.IngestionMappingType != DFUnknown && props.Ingestion.Additional.Format.MappingKind() != props.Ingestion.Additional.IngestionMappingType {
		return nil, properties.All{}, errors.ES(
			errors.OpUnknown,
			errors.KClientArgs,
			"format and ingestion mapping type must match (hint: using ingestion mapping sets the format automatically)",
		).SetNoRetry()
	}

	if props.Ingestion.ReportLevel != properties.None {
		if props.Source.ID == uuid.Nil {
			props.Source.ID = uuid.New()
		}

		switch props.Ingestion.ReportMethod {
		case properties.ReportStatusToTable, properties.ReportStatusToQueueAndTable:
			tableResources, err := i.mgr.GetTables()
			if err != nil {
				return nil, properties.All{}, err
			}

			if len(tableResources) == 0 {
				return nil, properties.All{}, fmt.Errorf("User requested reporting status to table, yet status table resource URI is not found")
			}

			props.Ingestion.TableEntryRef.TableConnectionString = tableResources[0].URL().String()
			props.Ingestion.TableEntryRef.PartitionKey = props.Source.ID.String()
			props.Ingestion.TableEntryRef.RowKey = uuid.Nil.String()
		}
	}

	result.putProps(props)
	return result, props, nil
}

// FromFile allows uploading a data file for Kusto from either a local path or a blobstore URI path.
// This method is thread-safe.
func (i *Ingestion) FromFile(ctx context.Context, fPath string, options ...FileOption) (*Result, error) {
	return i.fromFile(ctx, fPath, options, i.newProp())
}

// fromFile is an internal function to allow managed streaming to pass a properties object to the ingestion.
func (i *Ingestion) fromFile(ctx context.Context, fPath string, options []FileOption, props properties.All) (*Result, error) {
	local, err := queued.IsLocalPath(fPath)
	if err != nil {
		return nil, err
	}

	var scope SourceScope
	if local {
		scope = FromFile
		props.Source.OriginalSource = fPath
	} else {
		scope = FromBlob
	}

	result, props, err := i.prepForIngestion(ctx, options, props, scope)
	if err != nil {
		return nil, err
	}

	result.record.IngestionSourcePath = fPath

	if local {
		err = i.fs.Local(ctx, fPath, props)
	} else {
		err = i.fs.Blob(ctx, fPath, 0, props)
	}

	if err != nil {
		return nil, err
	}

	result.putQueued(ctx, i)
	return result, nil
}

// FromReader allows uploading a data file for Kusto from an io.Reader. The content is uploaded to Blobstore and
// ingested after all data in the reader is processed. Content should not use compression as the content will be
// compressed with gzip. This method is thread-safe.
func (i *Ingestion) FromReader(ctx context.Context, reader io.Reader, options ...FileOption) (*Result, error) {
	return i.fromReader(ctx, reader, options, i.newProp())
}

// fromReader is an internal function to allow managed streaming to pass a properties object to the ingestion.
func (i *Ingestion) fromReader(ctx context.Context, reader io.Reader, options []FileOption, props properties.All) (*Result, error) {
	result, props, err := i.prepForIngestion(ctx, options, props, FromReader)
	if err != nil {
		return nil, err
	}

	path, err := i.fs.Reader(ctx, reader, props)
	if err != nil {
		return nil, err
	}

	result.record.IngestionSourcePath = path
	result.putQueued(ctx, i)
	return result, nil
}

func (i *Ingestion) newProp() properties.All {
	return properties.All{
		Ingestion: properties.Ingestion{
			DatabaseName: i.db,
			TableName:    i.table,
		},
	}
}

func (i *Ingestion) Close() error {
	i.mgr.Close()
	err := i.client.Close()
	if err != nil {
		return err
	}
	err = i.fs.Close()
	return err
}
