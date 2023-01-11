package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

const (
	mb                     = 1024 * 1024
	maxStreamingSize       = 4 * mb
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

func (m *Managed) FromFile(ctx context.Context, fPath string, options ...FileOption) (*Result, error) {
	props := m.newProp()
	file, err := prepFileAndProps(fPath, &props, options, ManagedClient)

	if err == FileIsBlobErr { // Non-local file - fallback to queued
		return m.queued.fromFile(ctx, fPath, []FileOption{}, props)
	}

	if err != nil {
		return nil, err
	}

	return m.managedStreamImpl(ctx, file, props)
}

func (m *Managed) FromReader(ctx context.Context, reader io.Reader, options ...FileOption) (*Result, error) {
	props := m.newProp()

	for _, prop := range options {
		err := prop.Run(&props, ManagedClient, FromReader)
		if err != nil {
			return nil, err
		}
	}

	return m.managedStreamImpl(ctx, reader, props)
}

func (m *Managed) managedStreamImpl(ctx context.Context, payload io.Reader, props properties.All) (*Result, error) {
	compress := !props.Source.DontCompress
	if compress {
		payload = gzip.Compress(payload)
		props.Source.DontCompress = true
	}
	maxSize := maxStreamingSize

	buf, err := io.ReadAll(io.LimitReader(payload, int64(maxSize+1)))
	if err != nil {
		return nil, err
	}

	// If the payload is larger than the max size for streaming, we fall back to queued by combining what we read with the rest of the payload
	if len(buf) > maxSize {
		combinedBuf := io.MultiReader(bytes.NewReader(buf), payload)
		return m.queued.fromReader(ctx, combinedBuf, []FileOption{}, props)
	}

	var result *Result

	hasCustomId := props.Streaming.ClientRequestId != ""
	i := 0
	managedUuid := uuid.New().String()

	actualBackoff := backoff.WithContext(backoff.WithMaxRetries(props.ManagedStreaming.Backoff, retryCount), ctx)

	err = backoff.Retry(func() error {
		if !hasCustomId {
			props.Streaming.ClientRequestId = fmt.Sprintf("KGC.executeManagedStreamingIngest;%s;%d", managedUuid, i)
		}
		result, err = streamImpl(m.streaming.streamConn, ctx, bytes.NewReader(buf), props)
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

	// Fallback to queued
	if errors.Retry(err) {
		return m.queued.fromReader(ctx, bytes.NewReader(buf), []FileOption{}, props)
	}

	return nil, err
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
