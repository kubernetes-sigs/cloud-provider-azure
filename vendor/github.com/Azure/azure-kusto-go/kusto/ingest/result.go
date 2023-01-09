package ingest

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/status"
)

// Result provides a way for users track the state of ingestion jobs.
type Result struct {
	record        statusRecord
	tableClient   *status.TableClient
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

// putQueued sets the initial success status depending on status reporting state
func (r *Result) putQueued(mgr *resources.Manager) {
	// If not checking status, just return queued
	if !r.reportToTable {
		r.record.Status = Queued
		return
	}

	// Get table URI
	managerResources, err := mgr.Resources()
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed getting status table URI: " + err.Error()
		return
	}

	if len(managerResources.Tables) == 0 {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Ingestion resources do not include a status table URI: " + err.Error()
		return
	}

	// create a table client
	client, err := status.NewTableClient(*managerResources.Tables[0])
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed Creating a Status Table client: " + err.Error()
		return
	}

	// StreamIngest initial record
	r.record.Status = Pending
	err = client.Write(r.record.IngestionSourceID.String(), r.record.ToMap())
	if err != nil {
		r.record.Status = StatusRetrievalFailed
		r.record.FailureStatus = Permanent
		r.record.Details = "Failed writing initial status record: " + err.Error()
		return
	}

	r.tableClient = client
}

// Wait returns a channel that can be checked for ingestion results.
// In order to check actual status please use the ReportResultToTable option when ingesting data.
func (r *Result) Wait(ctx context.Context) chan error {
	ch := make(chan error, 1)

	if r.record.Status.IsFinal() || !r.reportToTable {
		close(ch)
		return ch
	}

	go func() {
		defer close(ch)

		r.poll(ctx)
		if !r.record.Status.IsSuccess() {
			ch <- r.record
		}
	}()

	return ch
}

func (r *Result) poll(ctx context.Context) {
	const pollInterval = 10 * time.Second
	attempts := 3
	delay := [3]int{120, 60, 10} // attempts are counted backwards

	// create a table client
	if r.tableClient != nil {
		// Create a ticker to poll the table in 10 second intervals.
		timer := time.NewTimer(pollInterval)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				r.record.Status = StatusRetrievalCanceled
				r.record.FailureStatus = Transient
				return

			case <-timer.C:
				smap, err := r.tableClient.Read(r.record.IngestionSourceID.String())
				if err != nil {
					if attempts == 0 {
						r.record.Status = StatusRetrievalFailed
						r.record.FailureStatus = Transient
						r.record.Details = "Failed reading from Status Table: " + err.Error()
						return
					}

					attempts = attempts - 1
					time.Sleep(time.Duration(delay[attempts]+rand.Intn(5)) * time.Second)
				} else {
					r.record.FromMap(smap)
					if r.record.Status.IsFinal() {
						return
					}
				}

				timer.Reset(pollInterval)
			}
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
