package azkustoingest

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Azure/azure-kusto-go/azkustoingest/ingestoptions"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/utils"
	"io"
	"os"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/properties"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/queued"
	"github.com/google/uuid"
)

type streamIngestor interface {
	io.Closer
	StreamIngest(ctx context.Context, db, table string, payload io.Reader, format azkustodata.DataFormatForStreaming, mappingName string, clientRequestId string, isBlobUri bool) error
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
func NewStreaming(kcsb *azkustodata.ConnectionStringBuilder, options ...Option) (*Streaming, error) {
	o := getOptions(options)

	if !o.withoutEndpointCorrection {
		newKcsb := *kcsb
		newKcsb.DataSource = removeIngestPrefix(newKcsb.DataSource)
		kcsb = &newKcsb
	}

	var client *azkustodata.Client
	var err error

	if o.httpClient != nil {
		client, err = azkustodata.New(kcsb, azkustodata.WithHttpClient(o.httpClient))
	} else {
		client, err = azkustodata.New(kcsb)
	}

	if err != nil {
		return nil, err
	}

	return newStreamingFromClient(client, o)
}

func newStreamingFromClient(client QueryClient, o *Ingestion) (*Streaming, error) {
	streamConn, err := azkustodata.NewConn(removeIngestPrefix(client.Endpoint()), client.Auth(), client.HttpClient(), client.ClientDetails())
	if err != nil {
		client.Close()
		return nil, err
	}

	i := &Streaming{
		db:         o.db,
		table:      o.table,
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

	if !local {
		return nil, nil, false
	}

	props.Source.OriginalSource = fPath
	compression := utils.CompressionDiscovery(fPath)
	err = queued.CompleteFormatFromFileName(props, fPath)
	if err != nil {
		return nil, err, true
	}

	props.Source.DontCompress = !queued.ShouldCompress(props, compression)

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
	compress := queued.ShouldCompress(&props, ingestoptions.CTUnknown)
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
