package ingest

import (
	"fmt"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	storageuid "github.com/gofrs/uuid"
	"github.com/google/uuid"
	"github.com/kylelemons/godebug/pretty"
)

// StatusCode is the ingestion status
type StatusCode string

//goland:noinspection GoUnusedConst - Part of the API
const (
	// Pending status represents a temporary status.
	// Might change during the course of ingestion based on the
	// outcome of the data ingestion operation into Kusto.
	Pending StatusCode = "Pending"
	// Succeeded status represents a permanent status.
	// The data has been successfully ingested to Kusto.
	Succeeded StatusCode = "Succeeded"
	// Failed Status represents a permanent status.
	// The data has not been ingested to Kusto.
	Failed StatusCode = "Failed"
	// Queued status represents a permanent status.
	// The data has been queued for ingestion &  status tracking was not requested.
	// (This does not indicate that the ingestion was successful.)
	Queued StatusCode = "Queued"
	// Skipped status represents a permanent status.
	// No data was supplied for ingestion. The ingest operation was skipped.
	Skipped StatusCode = "Skipped"
	// PartiallySucceeded status represents a permanent status.
	// Part of the data was successfully ingested to Kusto, while other parts failed.
	PartiallySucceeded StatusCode = "PartiallySucceeded"

	// StatusRetrievalFailed means the client ran into truble reading the status from the service
	StatusRetrievalFailed StatusCode = "StatusRetrievalFailed"
	// StatusRetrievalCanceled means the user canceld the status check
	StatusRetrievalCanceled StatusCode = "StatusRetrievalCanceled"
)

// IsFinal returns true if the ingestion status is a final status, or false if the status is temporary
func (i StatusCode) IsFinal() bool {
	return i != Pending
}

// IsSuccess returns true if the status code is a final successfull status code
func (i StatusCode) IsSuccess() bool {
	switch i {
	case Succeeded, Queued:
		return true

	default:
		return false
	}
}

// FailureStatusCode indicates the status of failed ingestion attempts
type FailureStatusCode string

const (
	// Unknown represents an undefined or unset failure state
	Unknown FailureStatusCode = "Unknown"
	// Permanent represnets failure state that will benefit from a retry attempt
	Permanent FailureStatusCode = "Permanent"
	// Transient represnet a retryable failure state
	Transient FailureStatusCode = "Transient"
	// Exhausted represents a retryable failure that has exhusted all retry attempts
	Exhausted FailureStatusCode = "Exhausted"
)

// IsRetryable indicates whether there's any merit in retying ingestion
func (i FailureStatusCode) IsRetryable() bool {
	switch i {
	case Transient, Exhausted:
		return true

	default:
		return false
	}
}

// statusRecord is a record containing information regarding the status of an ingestion command
type statusRecord struct {
	// Status is The ingestion status returned from the service. Status remains 'Pending' during the ingestion process and
	// is updated by the service once the ingestion completes. When <see cref="IngestionReportMethod"/> is set to 'Queue', the ingestion status
	// will always be 'Queued' and the caller needs to query the reports queues for ingestion status, as configured. To query statuses that were
	// reported to queue, see: <see href="https://docs.microsoft.com/en-us/azure/kusto/api/netfx/kusto-ingest-client-status#ingestion-status-in-azure-queue"/>.
	// When <see cref="IngestionReportMethod"/> is set to 'Table', call <see cref="IKustoIngestionResult.GetIngestionStatusBySourceId"/> or
	// <see cref="IKustoIngestionResult.GetIngestionStatusCollection"/> to retrieve the most recent ingestion status.
	Status StatusCode

	// IngestionSourceID is a unique identifier representing the ingested source. It can be supplied during the ingestion execution.
	IngestionSourceID uuid.UUID

	// IngestionSourcePath is the URI of the blob, potentially including the secret needed to access
	// the blob. This can be a filesystem URI (on-premises deployments only),
	// or an Azure Blob Storage URI (including a SAS key or a semicolon followed by the account key).
	IngestionSourcePath string

	// Database is the name of the database holding the target table.
	Database string

	// Table is the name of the target table into which the data will be ingested.
	Table string

	// UpdatedOn is the last updated time of the ingestion status.
	UpdatedOn time.Time

	// OperationID is the ingestion's operation ID.
	OperationID uuid.UUID

	// ActivityID is the ingestion's activity ID.
	ActivityID uuid.UUID

	// ErrorCode In case of a failure, indicates the failure's error code.
	ErrorCode string

	// FailureStatus - In case of a failure, indicates the failure's status.
	FailureStatus FailureStatusCode

	// Details is a human readable description of the error added in case of a failure.
	Details string

	// OriginatesFromUpdatePolicy indicates whether or not the failure originated from an Update Policy, in case of a failure.
	OriginatesFromUpdatePolicy bool
}

const (
	undefinedString = "Undefined"
	unknownString   = "Unknown"
)

// newStatusRecord creates a new record initialized with defaults.
func newStatusRecord() statusRecord {
	rec := statusRecord{
		Status:                     Failed,
		IngestionSourceID:          uuid.Nil,
		IngestionSourcePath:        undefinedString,
		Database:                   undefinedString,
		Table:                      undefinedString,
		UpdatedOn:                  time.Now(),
		OperationID:                uuid.Nil,
		ActivityID:                 uuid.Nil,
		ErrorCode:                  unknownString,
		FailureStatus:              Unknown,
		Details:                    "",
		OriginatesFromUpdatePolicy: false,
	}

	return rec
}

// FromProps takes in data from ingestion options.
func (r *statusRecord) FromProps(props properties.All) {
	r.IngestionSourceID = props.Source.ID
	r.Database = props.Ingestion.DatabaseName
	r.Table = props.Ingestion.TableName
	r.UpdatedOn = time.Now()

	if props.Ingestion.BlobPath != "" && r.IngestionSourcePath == undefinedString {
		r.IngestionSourcePath = properties.RemoveQueryParamsFromUrl(props.Ingestion.BlobPath)
	}
}

// FromMap converts an ingestion status record to a key value map.
func (r *statusRecord) FromMap(data map[string]interface{}) {
	strStatus := safeGetString(data, "Status")
	if len(strStatus) > 0 {
		r.Status = StatusCode(strStatus)
	}

	strStatus = safeGetString(data, "FailureStatus")
	if len(strStatus) > 0 {
		r.FailureStatus = FailureStatusCode(strStatus)
	}

	r.IngestionSourcePath = properties.RemoveQueryParamsFromUrl(safeGetString(data, "IngestionSourcePath"))
	r.Database = safeGetString(data, "Database")
	r.Table = safeGetString(data, "Table")
	r.ErrorCode = safeGetString(data, "ErrorCode")
	r.Details = safeGetString(data, "Details")

	r.IngestionSourceID = getGoogleUUIDFromInterface(data, "IngestionSourceId")
	r.OperationID = getGoogleUUIDFromInterface(data, "OperationId")
	r.ActivityID = getGoogleUUIDFromInterface(data, "ActivityId")

	if data["UpdatedOn"] != nil {
		if t, err := getTimeFromInterface(data["UpdatedOn"]); err == nil {
			r.UpdatedOn = t
		}
	}

	if data["OriginatesFromUpdatePolicy"] != nil {
		if b, ok := data["OriginatesFromUpdatePolicy"].(bool); ok {
			r.OriginatesFromUpdatePolicy = b
		}
	}
}

// StatusFromMapForTests converts an ingestion status record to a key value map. This is useful for comparison in tests.
func StatusFromMapForTests(data map[string]interface{}) error {
	r := newStatusRecord()
	r.FromMap(data)
	return r
}

// ToMap converts an ingestion status record to a key value map.
func (r *statusRecord) ToMap() map[string]interface{} {
	data := make(map[string]interface{})

	// Since we only create the initial record, It's not our responsibility to write the following fields:
	//   OperationID, AcitivityID, ErrorCode, FailureStatus, Details, OriginatesFromUpdatePolicy
	// Those will be read from the server if they have data in them
	data["Status"] = r.Status
	data["IngestionSourceId"] = r.IngestionSourceID
	data["IngestionSourcePath"] = properties.RemoveQueryParamsFromUrl(r.IngestionSourcePath)
	data["Database"] = r.Database
	data["Table"] = r.Table
	data["UpdatedOn"] = r.UpdatedOn.Format(time.RFC3339Nano)

	return data
}

// String implements fmt.Stringer.
func (r *statusRecord) String() string {
	return pretty.Sprint(r)
}

// Error converts an ingestion status to a string. Since we only provide the record in case of an error, the success branches will never be called.
func (r statusRecord) Error() string {
	switch r.Status {
	case Succeeded:
		return fmt.Sprintf("Ingestion succeeded\n" + r.String())

	case Queued:
		return fmt.Sprintf("Ingestion Queued\n" + r.String())

	case PartiallySucceeded:
		return fmt.Sprintf("Ingestion succeeded partially\n" + r.String())

	default:
		return fmt.Sprintf("Ingestion Failed\n" + r.String())
	}
}

func getTimeFromInterface(x interface{}) (time.Time, error) {
	switch x := x.(type) {
	case string:
		return time.Parse(time.RFC3339Nano, x)

	case time.Time:
		return x, nil

	default:
		return time.Now(), fmt.Errorf("getTimeFromInterface: Unexpected format %T", x)
	}
}

func getGoogleUUIDFromInterface(data map[string]interface{}, key string) uuid.UUID {
	x := data[key]

	if x == nil {
		return uuid.Nil
	}

	switch x := x.(type) {
	case uuid.UUID:
		return x

	case string:
		uid, err := uuid.Parse(x)
		if err != nil {
			return uuid.Nil
		}

		return uid

	case storageuid.UUID:
		uid, err := uuid.ParseBytes(x.Bytes())
		if err != nil {
			return uuid.Nil
		}

		return uid

	default:
		return uuid.Nil
	}
}

func safeGetString(data map[string]interface{}, key string) string {
	if v := data[key]; v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}

	return ""
}
