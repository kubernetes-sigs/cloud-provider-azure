// Package properties provides Kusto REST properties that will need to be serialized and sent to Kusto
// based upon the type of ingestion we are doing.
package properties

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustoingest/ingestoptions"
	"github.com/cenkalti/backoff/v4"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/google/uuid"
)

// DataFormat indicates what type of encoding format was used for source data.
// Note: This is very similar to azkustoingest.DataFormat, except this supports more formats.
// We are not using a shared list, because this list is used only internally and is for the
// data itself, not the mapping reference.  Structure prevents packages such as filesystem
// from importing ingest, because ingest imports filesystem.  We would end up with recursive imports.
// More info here: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
type DataFormat int

const (
	// DFUnknown indicates the EncodingType is not set.
	DFUnknown DataFormat = 0
	// AVRO indicates the source is encoded in Apache Avro format.
	AVRO DataFormat = 1
	// ApacheAVRO indicates the source is encoded in Apache avro2json format.
	ApacheAVRO DataFormat = 2
	// CSV indicates the source is encoded in comma seperated values.
	CSV DataFormat = 3
	// JSON indicates the source is encoded as one or more lines, each containing a record in Javascript Object Notation.
	JSON DataFormat = 4
	// MultiJSON indicates the source is encoded in JSON-Array of individual records in Javascript Object Notation. Optionally,
	//multiple documents can be concatenated.
	MultiJSON DataFormat = 5
	// ORC indicates the source is encoded in Apache Optimized Row Columnar format.
	ORC DataFormat = 6
	// Parquet indicates the source is encoded in Apache Parquet format.
	Parquet DataFormat = 7
	// PSV is pipe "|" separated values.
	PSV DataFormat = 8
	// Raw is a text file that has only a single string value.
	Raw DataFormat = 9
	// SCSV is a file containing semicolon ";" separated values.
	SCSV DataFormat = 10
	// SOHSV is a file containing SOH-separated values(ASCII codepoint 1).
	SOHSV DataFormat = 11
	// SStream indicats the source is encoded as a Microsoft Cosmos Structured Streams format
	SStream DataFormat = 12
	// TSV is a file containing tab seperated values ("\t").
	TSV DataFormat = 13
	// TSVE is a file containing escaped-tab seperated values ("\t").
	TSVE DataFormat = 14
	// TXT is a text file with lines delimited by "\n".
	TXT DataFormat = 15
	// W3CLogFile indicates the source is encoded using W3C Extended Log File format.
	W3CLogFile DataFormat = 16
	// SingleJSON indicates the source is a single JSON value -- newlines are regular whitespace.
	SingleJSON DataFormat = 17
)

type dfDescriptor struct {
	camelName      string
	jsonName       string
	detectableExt  string
	mappingKind    DataFormat
	shouldCompress bool
}

var dfDescriptions = []dfDescriptor{
	{"", "", "", DFUnknown, true},
	{"Avro", "avro", ".avro", AVRO, false},
	{"ApacheAvro", "avro", "", AVRO, false},
	{"Csv", "csv", ".csv", CSV, true},
	{"Json", "json", ".json", JSON, true},
	{"MultiJson", "multijson", "", JSON, true},
	{"Orc", "orc", ".orc", ORC, false},
	{"Parquet", "parquet", ".parquet", Parquet, false},
	{"Psv", "psv", ".psv", CSV, true},
	{"Raw", "raw", ".raw", CSV, true},
	{"Scsv", "scsv", ".scsv", CSV, true},
	{"Sohsv", "sohsv", ".sohsv", CSV, true},
	{"SStream", "sstream", ".ss", DFUnknown, false},
	{"Tsv", "tsv", ".tsv", CSV, true},
	{"Tsve", "tsve", ".tsve", CSV, true},
	{"Txt", "txt", ".txt", CSV, true},
	{"W3cLogFile", "w3clogfile", ".w3clogfile", W3CLogFile, true},
	{"SingleJson", "singlejson", "", JSON, true},
}

// IngestionReportLevel defines which ingestion statuses are reported by the DM.
type IngestionReportLevel int

//goland:noinspection GoUnusedConst - Part of the API
const (
	// FailuresOnly tells to the DM to report the ingestion sytatus of failed ingestions only.
	FailuresOnly IngestionReportLevel = 0
	// None tells to the DM not to report ingestion status.
	None IngestionReportLevel = 1
	// FailureAndSuccess tells to the DM to report ingestion status for failed and successfull ingestions.
	FailureAndSuccess IngestionReportLevel = 2
)

// IngestionReportMethod defines where the DM reports ingestion statuses to.
type IngestionReportMethod int

//goland:noinspection GoUnusedConst - Part of the API
const (
	// ReportStatusToQueue tells the DM to report ingestion status to the a queue.
	ReportStatusToQueue IngestionReportMethod = 0
	// ReportStatusToTable tells the DM to report ingestion status to the a table.
	ReportStatusToTable IngestionReportMethod = 1
	// ReportStatusToQueueAndTable tells the DM to report ingestion status to both queues and tables.
	ReportStatusToQueueAndTable IngestionReportMethod = 2
	// ReportStatusToAzureMonitoring tells the DM to report ingestion status to azure monitor.
	ReportStatusToAzureMonitoring IngestionReportMethod = 3
)

// String implements fmt.Stringer.
func (d DataFormat) String() string {
	if d > 0 && int(d) < len(dfDescriptions) {
		return dfDescriptions[d].jsonName
	}

	return ""
}

// CamelCase returns the CamelCase version. This is for internal use, do not use.
// This can be removed in future versions.
func (d DataFormat) CamelCase() string {
	if d > 0 && int(d) < len(dfDescriptions) {
		return dfDescriptions[d].camelName
	}

	return ""
}

func (d DataFormat) KnownOrDefault() azkustodata.DataFormatForStreaming {
	if d == DFUnknown {
		return CSV
	}

	return d
}

// MarshalJSON implements json.Marshaler.MarshalJSON.
func (d DataFormat) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return nil, fmt.Errorf("DataFormat is an invalid encoding type")
	}

	return []byte(fmt.Sprintf("%q", d.String())), nil
}

// MappingKind returns the mapping kind associated with this DataFormat```
func (d DataFormat) MappingKind() DataFormat {
	if int(d) < len(dfDescriptions) {
		return dfDescriptions[d].mappingKind
	}

	return DFUnknown
}

func (d DataFormat) ShouldCompress() bool {
	if d > 0 && int(d) < len(dfDescriptions) {
		return dfDescriptions[d].shouldCompress
	}

	return true
}

// DataFormatDiscovery looks at the file name and tries to discern what the file format is.
func DataFormatDiscovery(fName string) DataFormat {
	name := fName

	u, err := url.Parse(fName)
	if err == nil && u.Scheme != "" {
		name = u.Path
	}

	ext := strings.ToLower(filepath.Ext(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(name), ".zip"), ".gz")))

	if ext == "" {
		return DFUnknown
	}

	for i := 1; i < len(dfDescriptions); i++ {
		if ext == dfDescriptions[i].detectableExt {
			return DataFormat(i)
		}
	}

	return DFUnknown
}

// All holds the complete set of properties that might be used.
type All struct {
	// Ingestion is a set of properties that are used across all ingestion methods.
	Ingestion Ingestion
	// Source provides options that are used to operate on the source data.
	Source SourceOptions
	// Streaming provides options that are used when doing an ingestion from a stream.
	Streaming Streaming
	// ManagedStreaming provides options that are used when doing an ingestion from a ManagedStreaming client.
	ManagedStreaming ManagedStreaming
}

// ManagedStreaming provides options that are used when doing an ingestion from a ManagedStreaming client.
type ManagedStreaming struct {
	// Backoff is the backoff strategy to use when retrying a transiently failed ingestion.
	Backoff backoff.BackOff
}

// Streaming provides options that are used when doing a streaming ingestion.
type Streaming struct {
	// ClientRequestID is the client request ID to use for the ingestion.
	ClientRequestId string
}

// SourceOptions are options that the user provides about the source that is going to be uploaded.
type SourceOptions struct {
	// ID allows someone to set the UUID for upload themselves. We aren't providing this option at this time, but here
	// when we do.
	ID uuid.UUID

	// DeleteLocalSource indicates to delete the local file after it has been consumed.
	DeleteLocalSource bool

	// DontCompress indicates to not compress the file. In streaming - do not pass DontCompress if file is not already compressed.
	DontCompress bool

	// OriginalSource is the path to the original source file, used for deletion.
	OriginalSource string

	// CompressionType is the type of compression used on the file.
	CompressionType ingestoptions.CompressionType
}

// Ingestion is a JSON serializable set of options that must be provided to the service.
type Ingestion struct {
	// ID is the unqique UUID for this upload.
	ID uuid.UUID `json:"Id"`
	// BlobPath is the URI representing the blob.
	BlobPath string
	// DatabaseName is the name of the Kusto database the data will ingest into.
	DatabaseName string
	// TableName is the name of the Kusto table the the data will ingest into.
	TableName string
	// RawDataSize is the uncompressed data size. Should be used to comunicate the file size to the service for efficient ingestion.
	RawDataSize int64 `json:",omitempty"`
	// RetainBlobOnSuccess indicates if the source blob should be retained or deleted. True is preferrable.
	RetainBlobOnSuccess bool `json:",omitempty"`
	// FlushImmediately - the service batching manager will not aggregate this file, thus overriding the batching policy
	FlushImmediately bool
	// IgnoreSizeLimit - ignores the size limit for data ingestion.
	IgnoreSizeLimit bool `json:",omitempty"`
	// ReportLevel defines which if any ingestion states are reported.
	ReportLevel IngestionReportLevel `json:",omitempty"`
	// ReportMethod defines which mechanisms are used to report the ingestion status.
	ReportMethod IngestionReportMethod `json:",omitempty"`
	// SourceMessageCreationTime is when we created the blob.
	SourceMessageCreationTime time.Time `json:",omitempty"`
	// Additional (properties) is a set of extra properties added to the ingestion command.
	Additional Additional `json:"AdditionalProperties"`
	// TableEntryRef points to the staus table entry used to report the status of this ingestion.
	TableEntryRef StatusTableDescription `json:"IngestionStatusInTable,omitempty"`
	// ApplicationForTracing is the application name that is used for tracing.
	ApplicationForTracing string `json:",omitempty"`
	// ClientVersionForTracing is the client version that is used for tracing.
	ClientVersionForTracing string `json:",omitempty"`
}

// Additional is additional properites.
type Additional struct {
	// AuthContext is the authorization string that we get from resources.Manager.AuthContext().
	AuthContext string `json:"authorizationContext,omitempty"`
	// IngestionMapping is a json string that maps the data being imported to the table's columns.
	// See: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
	IngestionMapping string `json:"ingestionMapping,omitempty"`
	// IngestionMappingRef is a string representing a mapping reference that has been uploaded to the server
	// via a Mgmt() call. See: https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
	IngestionMappingRef string `json:"ingestionMappingReference,omitempty"`
	// IngestionMappingType is what the mapping reference is encoded in: csv, json, avro, ...
	IngestionMappingType DataFormat `json:"ingestionMappingType,omitempty"`
	// ValidationPolicy is a JSON encoded string that tells our ingestion action what policies we want on the
	// data being ingested and what to do when that is violated.
	ValidationPolicy  string     `json:"validationPolicy,omitempty"`
	Format            DataFormat `json:"format,omitempty"`
	IgnoreFirstRecord bool       `json:"ignoreFirstRecord"`
	// Tags is a list of tags to associated with the ingested data.
	Tags []string `json:"tags,omitempty"`
	// IngestIfNotExists is a string value that, if specified, prevents ingestion from succeeding if the table already
	// has data tagged with an ingest-by: tag with the same value. This ensures idempotent data ingestion.
	IngestIfNotExists string `json:"ingestIfNotExists,omitempty"`
	// CreationTime is used to override the time considered for retantion policies, which by default is the time of ingestion.
	CreationTime time.Time `json:"creationTime,omitempty"`
}

// StatusTableDescription is a reference to the table status entry used for this ingestion command.
type StatusTableDescription struct {
	// TableConnectionString is a secret-free connection string to the status table.
	TableConnectionString string `json:",omitempty"`
	// PartitionKey is the partition key of the table entry.
	PartitionKey string `json:",omitempty"`
	// RowKey is the row key of the table entry.
	RowKey string `json:",omitempty"`
}

func (p *All) ApplyDeleteLocalSourceOption() error {
	if p.Source.DeleteLocalSource && p.Source.OriginalSource != "" {
		if err := os.Remove(p.Source.OriginalSource); err != nil {
			return errors.ES(errors.OpFileIngest, errors.KLocalFileSystem, "file was uploaded successfully, but we could not delete the local file: %s",
				err).SetNoRetry()
		}
	}
	return nil
}

// MarshalJSON implements json.Marshaller. This is for use only by the SDK and may be removed at any time.
func (a Additional) MarshalJSON() ([]byte, error) {
	// TODO(daniel): Have the backend fixed.
	// OK: This is here because in .Net DataFormat and IngestionMappingType are two different enumerators.
	// For some reason, they encode the values in two different ways and do exact string matches on the server.
	// So you must use "csv" and "Csv". For the moment, until we can get a backend change, we have to encode these
	// differently. I don't want to have two enumerators for the same thing, so I've done this hack to get around it.

	type additional2 Additional

	b, err := json.Marshal(additional2(a))
	if err != nil {
		return nil, err
	}

	m := map[string]interface{}{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}

	if _, ok := m["ingestionMappingType"]; ok {
		m["ingestionMappingType"] = a.IngestionMappingType.CamelCase()
	}

	return json.Marshal(m)
}

// MarshalJSONString will marshal Ingestion into a base64 encoded string.
func (i Ingestion) MarshalJSONString() (base64String string, err error) {
	i = i.defaults()
	if err := i.validate(); err != nil {
		return "", err
	}

	j, err := json.Marshal(i)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(j), nil
}

// defaults sets default values that can be auto-generated if not set. This is used inside our MarshalJSONString().
func (i Ingestion) defaults() Ingestion {
	if uuidIsZero(i.ID) {
		i.ID = uuid.New()
	}

	if i.SourceMessageCreationTime.IsZero() {
		i.SourceMessageCreationTime = time.Now()
	}

	return i
}

func (i Ingestion) validate() error {
	if uuidIsZero(i.ID) {
		return fmt.Errorf("the ID cannot be an zero value UUID")
	}
	switch "" {
	case i.DatabaseName:
		return fmt.Errorf("the database name cannot be an empty string")
	case i.TableName:
		return fmt.Errorf("the table name cannot be an empty string")
	case i.Additional.AuthContext:
		return fmt.Errorf("the authorization context was an empty string, which is not allowed")
	case i.BlobPath:
		return fmt.Errorf("the BlobPath was not set")
	}
	return nil
}

func RemoveQueryParamsFromUrl(url string) string {
	result := url
	if idx := strings.Index(result, "?"); idx != -1 {
		result = result[:idx]
	}
	if idx := strings.Index(result, ";"); idx != -1 {
		result = result[:idx]
	}
	return result
}

func uuidIsZero(id uuid.UUID) bool {
	for _, b := range id {
		if b != 0 {
			return false
		}
	}
	return true
}
