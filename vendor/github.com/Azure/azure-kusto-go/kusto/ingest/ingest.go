package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/Azure/azure-kusto-go/kusto"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/queued"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
	"github.com/google/uuid"
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

	connMu     sync.Mutex
	streamConn streamIngestor

	bufferSize int
	maxBuffers int
}

// Option is an optional argument to New().
type Option func(s *Ingestion)

// WithStaticBuffer configures the ingest client to upload data to Kusto using a set of one or more static memory buffers with a fixed size.
func WithStaticBuffer(bufferSize int, maxBuffers int) Option {
	return func(s *Ingestion) {
		s.bufferSize = bufferSize
		s.maxBuffers = maxBuffers
	}
}

// New is a constructor for Ingestion.
func New(client QueryClient, db, table string, options ...Option) (*Ingestion, error) {
	mgr, err := resources.New(client)
	if err != nil {
		return nil, err
	}

	i := &Ingestion{
		client: client,
		mgr:    mgr,
		db:     db,
		table:  table,
	}

	for _, option := range options {
		option(i)
	}

	fs, err := queued.New(db, table, mgr, client.HttpClient(), queued.WithStaticBuffer(i.bufferSize, i.maxBuffers))
	if err != nil {
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

	if props.Ingestion.Additional.IngestionMappingType != DFUnknown && props.Ingestion.Additional.Format != props.Ingestion.Additional.IngestionMappingType {
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

	result.putQueued(i.mgr)
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
	result.putQueued(i.mgr)
	return result, nil
}

// Deprecated: Stream use a streaming ingest client instead - `ingest.NewStreaming`.
// takes a payload that is encoded in format with a server stored mappingName, compresses it and uploads it to Kusto.
// More information can be found here:
// https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
// The context object can be used with a timeout or cancel to limit the request time.
func (i *Ingestion) Stream(ctx context.Context, payload []byte, format DataFormat, mappingName string) error {
	c, err := i.getStreamConn()
	if err != nil {
		return err
	}

	props := properties.All{
		Ingestion: properties.Ingestion{
			DatabaseName: i.db,
			TableName:    i.table,
			Additional: properties.Additional{
				Format:              format,
				IngestionMappingRef: mappingName,
			},
		},
	}

	_, err = streamImpl(c, ctx, bytes.NewReader(payload), props, false)

	return err
}

func (i *Ingestion) getStreamConn() (streamIngestor, error) {
	i.connMu.Lock()
	defer i.connMu.Unlock()

	if i.streamConn != nil {
		return i.streamConn, nil
	}

	sc, err := kusto.NewConn(removeIngestPrefix(i.client.Endpoint()), i.client.Auth(), i.client.HttpClient(), i.client.ClientDetails())
	if err != nil {
		return nil, err
	}
	i.streamConn = sc
	return i.streamConn, nil
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
	var err error
	err = i.fs.Close()
	if i.streamConn != nil {
		err2 := i.streamConn.Close()
		if err == nil {
			err = err2
		} else {
			err = errors.GetCombinedError(err, err2)
		}
	}
	return err
}
