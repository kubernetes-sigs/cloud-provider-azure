package azkustoingest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/Azure/azure-kusto-go/azkustoingest/internal/properties"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/status"
)

// Result provides a way for users track the state of ingestion jobs.
type Result struct {
	record        statusRecord
	tableClient   status.TableClientReader
	reportToTable bool
}

// newResult creates an initial ingestion status record.
func newResult() *Result {
	ret := &Result{}

	ret.record = newStatusRecord()
	return ret
}

// putProps sets the record to a failure state and adds the error to the record details.
func (r *Result) putProps(props properties.All) {
	r.reportToTable = props.Ingestion.ReportMethod == properties.ReportStatusToTable || props.Ingestion.ReportMethod == properties.ReportStatusToQueueAndTable
	r.record.FromProps(props)
}

// putQueued sets the initial success status depending on the status reporting state, returning the record on failure.
func (r *Result) putQueued(ctx context.Context, i *Ingestion) error {
	// If not checking status, just return queued
	if !r.reportToTable {
		r.record.Status = Queued
		return nil
	}

	// Get table URI
	tableResources, err := i.mgr.GetTables()
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed getting status table URI: " + err.Error()
		return &r.record
	}

	if len(tableResources) == 0 {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Ingestion resources does not include a status table URI."
		return &r.record
	}

	// create a table client
	client, err := status.NewTableClient(i.client.HttpClient(), *tableResources[0])
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed Creating a Status Table client: " + err.Error()
		return &r.record
	}

	// StreamIngest initial record
	r.record.Status = Pending
	err = client.Write(ctx, r.record.IngestionSourceID.String(), r.record.ToMap())
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed writing initial status record: " + err.Error()
		return &r.record
	}

	r.tableClient = client
	return nil
}

type waitConfig struct {
	interval           time.Duration
	immediateFirst     bool
	retryBackoffDelay  []time.Duration
	retryBackoffJitter time.Duration
}

type WaitOption func(o *waitConfig)

func WithImmediateFirst() WaitOption {
	return func(o *waitConfig) {
		o.immediateFirst = true
	}
}

func WithInterval(interval time.Duration) WaitOption {
	return func(o *waitConfig) {
		o.interval = interval
	}
}

func WithRetryBackoffDelay(delay ...time.Duration) WaitOption {
	return func(o *waitConfig) {
		o.retryBackoffDelay = delay
	}
}

func WithRetryBackoffJitter(jitter time.Duration) WaitOption {
	return func(o *waitConfig) {
		o.retryBackoffJitter = jitter
	}
}

var (
	DefaultWaitPollInterval           = 10 * time.Second
	DefaultWaitPollRetryBackoffDelay  = []time.Duration{10 * time.Second, 60 * time.Second, 120 * time.Second}
	DefaultWaitPollRetryBackoffJitter = 5 * time.Second
)

// Wait returns a channel that can be checked for ingestion results.
// In order to check actual status please use the ReportResultToTable option when ingesting data.
func (r *Result) Wait(ctx context.Context, options ...WaitOption) <-chan error {
	cfg := waitConfig{
		interval:           DefaultWaitPollInterval,
		retryBackoffDelay:  DefaultWaitPollRetryBackoffDelay,
		retryBackoffJitter: DefaultWaitPollRetryBackoffJitter,
	}

	for _, o := range options {
		o(&cfg)
	}

	ch := make(chan error, 1)

	if r.record.Status.IsFinal() {
		if !r.record.Status.IsSuccess() {
			ch <- r.record
		}
		close(ch)
		return ch
	}

	if !r.reportToTable {
		ch <- errors.New("status reporting is not enabled")
		close(ch)
		return ch
	}

	if r.tableClient == nil {
		ch <- errors.New("table client is not initialized")
		close(ch)
		return ch
	}

	go func() {
		defer close(ch)

		r.poll(ctx, &cfg)
		if !r.record.Status.IsSuccess() {
			ch <- r.record
		}
	}()

	return ch
}

func (r *Result) poll(ctx context.Context, cfg *waitConfig) {
	initialInterval := cfg.interval
	if cfg.immediateFirst {
		initialInterval = 0
	}
	attempts := cfg.retryBackoffDelay[:]
	timer := time.NewTimer(initialInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.record.Status = StatusRetrievalCanceled
			r.record.FailureStatus = Transient
			return

		case <-timer.C:
			smap, err := r.tableClient.Read(ctx, r.record.IngestionSourceID.String())
			sleepTime := cfg.interval
			if err != nil {
				if len(attempts) == 0 {
					r.record.Status = StatusRetrievalFailed
					r.record.FailureStatus = Transient
					r.record.Details = "Failed reading from Status Table: " + err.Error()
					return
				}

				sleepTime += attempts[0]
				attempts = attempts[1:]
				if cfg.retryBackoffJitter > 0 {
					sleepTime += time.Duration(rand.Intn(int(cfg.retryBackoffJitter)))
				}
			} else {
				r.record.FromMap(smap)
				if r.record.Status.IsFinal() {
					return
				}
			}

			timer.Reset(sleepTime)
		}
	}
}

// IsStatusRecord verifies that the given error is a status record.
func IsStatusRecord(err error) bool {
	_, ok := err.(statusRecord)
	return ok
}

// GetIngestionStatus extracts the ingestion status code from an ingestion error
func GetIngestionStatus(err error) (StatusCode, error) {
	if s, ok := err.(statusRecord); ok {
		return s.Status, nil
	}

	return Failed, fmt.Errorf("Error is not an Ingestion Result")
}

// GetIngestionFailureStatus extracts the ingestion failure code from an ingestion error
func GetIngestionFailureStatus(err error) (FailureStatusCode, error) {
	if s, ok := err.(statusRecord); ok {
		return s.FailureStatus, nil
	}

	return Unknown, fmt.Errorf("Error is not an Ingestion Result")
}

// GetErrorCode extracts the error code from an ingestion error
func GetErrorCode(err error) (string, error) {
	if s, ok := err.(statusRecord); ok {
		return s.ErrorCode, nil
	}

	return "", fmt.Errorf("Error is not an Ingestion Result")
}

// IsRetryable indicates whether there's any merit in retying ingestion
func IsRetryable(err error) bool {
	if s, ok := err.(statusRecord); ok {
		return s.FailureStatus.IsRetryable()
	}

	return false
}
