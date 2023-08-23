package ingest

import (
	// "bytes"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/queued"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/utils"

	"github.com/google/uuid"
)

type streamIngestor interface {
	io.Closer
	StreamIngest(ctx context.Context, db, table string, payload io.Reader, format kusto.DataFormatForStreaming, mappingName string, clientRequestId string, isBlobUri bool) error
}

// Streaming provides data ingestion from external sources into Kusto.
type Streaming struct {
	db         string
	table      string
	client     QueryClient
	streamConn streamIngestor
}

type blobUri struct {
	SourceUri string `json:"sourceUri"`
}

// NewStreaming is the constructor for Streaming.
// More information can be found here:
// https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
func NewStreaming(client QueryClient, db, table string) (*Streaming, error) {
	streamConn, err := kusto.NewConn(removeIngestPrefix(client.Endpoint()), client.Auth(), client.HttpClient(), client.ClientDetails())
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
	file, err, local := prepFileAndProps(fPath, &props, options, StreamingClient)

	if err != nil {
		return nil, err
	}

	if !local {
		return streamImpl(i.streamConn, ctx, generateBlobUriPayloadReader(fPath), props, true)
	}

	defer file.Close()
	return streamImpl(i.streamConn, ctx, file, props, false)
}

// Returns the opened file, err, boolean indicator if its a local file
func prepFileAndProps(fPath string, props *properties.All, options []FileOption, client ClientScope) (*os.File, error, bool) {
	var err error
	for _, option := range options {
		err := option.Run(props, client, FromFile)
		if err != nil {
			return nil, err, true
		}
	}

	local, err := queued.IsLocalPath(fPath)
	if err != nil {
		return nil, err, local
	}

	props.Source.OriginalSource = fPath

	if !local {
		return nil, nil, false
	}

	compression := utils.CompressionDiscovery(fPath)
	if compression != properties.CTNone {
		props.Source.DontCompress = true
	}

	err = queued.CompleteFormatFromFileName(props, fPath)
	if err != nil {
		return nil, err, true
	}

	file, err := os.Open(fPath)
	if err != nil {
		return nil, err, true
	}
	return file, nil, true
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

	return streamImpl(i.streamConn, ctx, reader, props, false)
}

func streamImpl(c streamIngestor, ctx context.Context, payload io.Reader, props properties.All, isBlobUri bool) (*Result, error) {
	compress := !props.Source.DontCompress
	if compress && !isBlobUri {
		payload = gzip.Compress(payload)
	}

	if props.Ingestion.Additional.Format == DFUnknown {
		props.Ingestion.Additional.Format = CSV
	}

	err := c.StreamIngest(ctx, props.Ingestion.DatabaseName, props.Ingestion.TableName, payload, props.Ingestion.Additional.Format,
		props.Ingestion.Additional.IngestionMappingRef,
		props.Streaming.ClientRequestId,
		isBlobUri)

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

func generateBlobUriPayloadReader(fPath string) io.Reader {
	buf := new(bytes.Buffer)
	json.NewEncoder(buf).Encode(
		blobUri{
			SourceUri: fPath,
		},
	)
	return io.NopCloser(buf)
}
