package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/ingestoptions"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/queued"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/utils"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

const (
	mb                     = 1024 * 1024
	maxStreamingSize       = int64(4 * mb)
	defaultInitialInterval = 1 * time.Second
	defaultMultiplier      = 2
	retryCount             = 2
)

type Managed struct {
	queued    *Ingestion
	streaming *Streaming
}

// NewManaged is a constructor for Managed.
func NewManaged(client QueryClient, db, table string, options ...Option) (*Managed, error) {
	queued, err := New(client, db, table, options...)
	if err != nil {
		return nil, err
	}
	streaming, err := NewStreaming(client, db, table)
	if err != nil {
		return nil, err
	}

	return &Managed{
		queued:    queued,
		streaming: streaming,
	}, nil
}

// Attempts to stream with retries, on success - return res,nil.
// If failed permanently - return err,nil.
// If failed transiently - return nil,nil.
func (m *Managed) streamWithRetries(ctx context.Context, payloadProvider func() io.Reader, props properties.All, isBlobUri bool) (*Result, error) {
	var result *Result

	hasCustomId := props.Streaming.ClientRequestId != ""
	i := 0
	managedUuid := uuid.New().String()

	actualBackoff := backoff.WithContext(backoff.WithMaxRetries(props.ManagedStreaming.Backoff, retryCount), ctx)

	var err error = nil
	err = backoff.Retry(func() error {
		if !hasCustomId {
			props.Streaming.ClientRequestId = fmt.Sprintf("KGC.executeManagedStreamingIngest;%s;%d", managedUuid, i)
		}
		result, err = streamImpl(m.streaming.streamConn, ctx, payloadProvider(), props, isBlobUri)
		i++
		if err != nil {
			if e, ok := err.(*errors.Error); ok {
				if errors.Retry(e) {
					return err
				} else {
					return backoff.Permanent(err)
				}
			} else {
				return backoff.Permanent(err)
			}
		}
		return nil
	}, actualBackoff)

	if err == nil {
		return result, nil
	}

	if errors.Retry(err) {
		// Caller should fallback to queued
		return nil, nil
	}

	return nil, err
}

func (m *Managed) FromFile(ctx context.Context, fPath string, options ...FileOption) (*Result, error) {
	props := m.newProp()
	file, err, local := prepFileAndProps(fPath, &props, options, ManagedClient)
	if err != nil {
		return nil, err
	}

	if !local {
		var size int64
		var compressionTypeForEstimation ingestoptions.CompressionType
		if size = props.Ingestion.RawDataSize; size == 0 {
			size, err = utils.FetchBlobSize(fPath, ctx, m.queued.client.HttpClient())
			if err != nil {
				// Failed fetch blob properties
				return nil, err
			}
			compressionTypeForEstimation = utils.CompressionDiscovery(fPath)
			props.Ingestion.RawDataSize = utils.EstimateRawDataSize(compressionTypeForEstimation, size)
		} else {
			// If user sets raw data size we always want to devide it for estimation
			compressionTypeForEstimation = ingestoptions.CTNone
		}

		// File is not compressed and user says its compressed, raw 10 mb -> do
		if !shouldUseQueuedIngestBySize(compressionTypeForEstimation, size) {
			res, err := m.streamWithRetries(ctx, func() io.Reader { return generateBlobUriPayloadReader(fPath) }, props, true)
			if err != nil || res != nil {
				return res, err
			}
		}

		return m.queued.fromFile(ctx, fPath, []FileOption{}, props)
	}

	// No need to get local file size as we later use the compressed stream size
	return m.managedStreamImpl(ctx, file, props)
}

func shouldUseQueuedIngestBySize(compression ingestoptions.CompressionType, fileSize int64) bool {
	switch compression {
	case ingestoptions.GZIP, ingestoptions.ZIP:
		return fileSize > maxStreamingSize
	}

	return fileSize/utils.EstimatedCompressionFactor > maxStreamingSize
}

func (m *Managed) FromReader(ctx context.Context, reader io.Reader, options ...FileOption) (*Result, error) {
	props := m.newProp()

	for _, prop := range options {
		err := prop.Run(&props, ManagedClient, FromReader)
		if err != nil {
			return nil, err
		}
	}

	return m.managedStreamImpl(ctx, io.NopCloser(reader), props)
}

func (m *Managed) managedStreamImpl(ctx context.Context, payload io.ReadCloser, props properties.All) (*Result, error) {
	defer payload.Close()
	compress := queued.ShouldCompress(&props, ingestoptions.CTUnknown)
	var compressed io.Reader = payload
	if compress {
		compressed = gzip.Compress(io.NopCloser(payload))
		props.Source.DontCompress = true
	}

	maxSize := maxStreamingSize

	buf, err := io.ReadAll(io.LimitReader(compressed, int64(maxSize+1)))
	if err != nil {
		return nil, err
	}

	if shouldUseQueuedIngestBySize(ingestoptions.GZIP, int64(len(buf))) {
		combinedBuf := io.MultiReader(bytes.NewReader(buf), compressed)
		return m.queued.fromReader(ctx, combinedBuf, []FileOption{}, props)
	}

	res, err := m.streamWithRetries(ctx, func() io.Reader { return bytes.NewReader(buf) }, props, false)
	if err != nil || res != nil {
		return res, err
	}

	// Theres no size estimation when ingesting from stream. If we did not already use queued ingestion
	// we can assume all the original payload reader is < 4mb, therefore no need to combine
	return m.queued.fromReader(ctx, bytes.NewReader(buf), []FileOption{}, props)
}

func (m *Managed) newProp() properties.All {
	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = defaultInitialInterval
	exp.Multiplier = defaultMultiplier

	return properties.All{
		Ingestion: properties.Ingestion{
			DatabaseName: m.streaming.db,
			TableName:    m.streaming.table,
		},
		ManagedStreaming: properties.ManagedStreaming{
			Backoff: exp,
		},
	}
}

func (m *Managed) Close() error {
	var err error
	err = m.queued.Close()
	if err2 := m.streaming.Close(); err2 != nil {
		err = errors.GetCombinedError(err, err2)
	}
	return err
}
