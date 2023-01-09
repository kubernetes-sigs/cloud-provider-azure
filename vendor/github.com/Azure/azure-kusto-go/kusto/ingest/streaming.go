package ingest

import (
	"context"
	"io"
	"os"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/conn"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/queued"
	"github.com/google/uuid"
)

type streamIngestor interface {
	io.Closer
	StreamIngest(ctx context.Context, db, table string, payload io.Reader, format properties.DataFormat, mappingName string, clientRequestId string) error
}

// Streaming provides data ingestion from external sources into Kusto.
type Streaming struct {
	db         string
	table      string
	client     QueryClient
	streamConn streamIngestor
}

var FileIsBlobErr = errors.ES(errors.OpIngestStream, errors.KClientArgs, "blobstore paths are not supported for streaming")

// NewStreaming is the constructor for Streaming.
// More information can be found here:
// https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
func NewStreaming(client QueryClient, db, table string) (*Streaming, error) {
	streamConn, err := conn.New(client.Endpoint(), client.Auth(), client.HttpClient())
	if err != nil {
		return nil, err
	}

	i := &Streaming{
		db:         db,
		table:      table,
		client:     client,
		streamConn: streamConn,
	}

	return i, nil
}

// FromFile allows uploading a data file for Kusto from either a local path or a blobstore URI path.
// This method is thread-safe.
func (i *Streaming) FromFile(ctx context.Context, fPath string, options ...FileOption) (*Result, error) {
	props := i.newProp()
	file, err := prepFileAndProps(fPath, &props, options, StreamingClient)
	if err != nil {
		return nil, err
	}

	return streamImpl(i.streamConn, ctx, file, props)
}

func prepFileAndProps(fPath string, props *properties.All, options []FileOption, client ClientScope) (*os.File, error) {
	local, err := queued.IsLocalPath(fPath)
	if err != nil {
		return nil, err
	}

	for _, option := range options {
		err := option.Run(props, client, FromFile)
		if err != nil {
			return nil, err
		}
	}

	if !local {
		return nil, FileIsBlobErr
	}

	props.Source.OriginalSource = fPath

	compression := queued.CompressionDiscovery(fPath)
	if compression != properties.CTNone {
		props.Source.DontCompress = true
	}

	err = queued.CompleteFormatFromFileName(props, fPath)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(fPath)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// FromReader allows uploading a data file for Kusto from an io.Reader. The content is uploaded to Blobstore and
// ingested after all data in the reader is processed. Content should not use compression as the content will be
// compressed with gzip. This method is thread-safe.
func (i *Streaming) FromReader(ctx context.Context, reader io.Reader, options ...FileOption) (*Result, error) {
	props := i.newProp()

	for _, prop := range options {
		err := prop.Run(&props, StreamingClient, FromReader)
		if err != nil {
			return nil, err
		}
	}

	return streamImpl(i.streamConn, ctx, reader, props)
}

func streamImpl(c streamIngestor, ctx context.Context, payload io.Reader, props properties.All) (*Result, error) {
	compress := !props.Source.DontCompress
	if compress {
		payload = gzip.Compress(payload)
	}

	if props.Ingestion.Additional.Format == DFUnknown {
		props.Ingestion.Additional.Format = CSV
	}

	err := c.StreamIngest(ctx, props.Ingestion.DatabaseName, props.Ingestion.TableName, payload, props.Ingestion.Additional.Format,
		props.Ingestion.Additional.IngestionMappingRef,
		props.Streaming.ClientRequestId)

	if err != nil {
		if e, ok := errors.GetKustoError(err); ok {
			return nil, e
		}
		return nil, errors.E(errors.OpIngestStream, errors.KClientArgs, err)
	}

	err = props.ApplyDeleteLocalSourceOption()
	if err != nil {
		return nil, err
	}

	result := newResult()
	result.putProps(props)
	result.record.Status = "Success"

	return result, nil
}

func (i *Streaming) newProp() properties.All {
	return properties.All{
		Ingestion: properties.Ingestion{
			DatabaseName: i.db,
			TableName:    i.table,
		},
		Streaming: properties.Streaming{
			ClientRequestId: "KGC.executeStreaming;" + uuid.New().String(),
		},
	}
}

func (i *Streaming) Close() error {
	return i.streamConn.Close()
}
