package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/ingestoptions"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/cenkalti/backoff/v4"
)

type SourceScope uint

type ClientScope uint

const (
	FromFile SourceScope = 1 << iota
	FromReader
	FromBlob
	QueuedClient ClientScope = 1 << iota
	StreamingClient
	ManagedClient
)

func (s SourceScope) String() string {
	switch s {
	case FromFile:
		return "FromFile"
	case FromReader:
		return "FromReader"
	case FromBlob:
		return "FromBlob"
	default:
		panic(fmt.Sprintf("unknown SourceScope %d", s))
	}
}

func (s ClientScope) String() string {
	switch s {
	case QueuedClient:
		return "QueuedClient"
	case StreamingClient:
		return "StreamingClient"
	default:
		panic(fmt.Sprintf("unknown ClientScope %d", s))
	}
}

// FileOption is an optional argument to FromFile().
type FileOption interface {
	fmt.Stringer

	SourceScopes() SourceScope
	ClientScopes() ClientScope

	Run(p *properties.All, clientType ClientScope, sourceType SourceScope) error
}

type option struct {
	run          func(p *properties.All) error
	clientScopes ClientScope
	sourceScope  SourceScope
	name         string
}

func (o option) SourceScopes() SourceScope {
	return o.sourceScope
}

func (o option) ClientScopes() ClientScope {
	return o.clientScopes
}

func (o option) String() string {
	return o.name
}

func (o option) Run(p *properties.All, clientType ClientScope, sourceType SourceScope) error {
	errType := errors.OpFileIngest
	if clientType&StreamingClient != 0 {
		errType = errors.OpIngestStream
	}

	if o.clientScopes&clientType == 0 {
		return errors.ES(errType, errors.KClientArgs, fmt.Sprintf("%s is not valid for client '%s'", o.name, clientType))
	}

	if o.sourceScope&sourceType == 0 {
		return errors.ES(errType, errors.KClientArgs, fmt.Sprintf("%s is not valid for ingestion source type '%s' for client '%s'", o.name, sourceType, clientType))
	}

	return o.run(p)
}

// Database overrides the default database name.
func Database(name string) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.DatabaseName = name
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "Database",
	}
}

// Table overrides the default table name.
func Table(name string) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.TableName = name
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "Table",
	}
}

// DontCompress sets whether to compress the data. 	In streaming - do not pass DontCompress if file is not already compressed.

func DontCompress() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Source.DontCompress = true
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile | FromReader,
		name:         "DontCompress",
	}
}

func backOff(off *backoff.ExponentialBackOff) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.ManagedStreaming.Backoff = off
			return nil
		},
		clientScopes: ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "BackOff",
	}
}

// FlushImmediately  the service batching manager will not aggregate this file, thus overriding the batching policy
func FlushImmediately() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.FlushImmediately = true
			return nil
		},
		clientScopes: QueuedClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "FlushImmediately",
	}
}

// IgnoreFirstRecord tells Kusto to flush on write.
func IgnoreFirstRecord() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.Additional.IgnoreFirstRecord = true
			return nil
		},
		clientScopes: QueuedClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "IgnoreFirstRecord",
	}
}

// DataFormat indicates what type of encoding format was used for source data.
// Not all options can be used in every method.
type DataFormat = properties.DataFormat

// note: any change here needs to be kept up to date with the properties version.
// I'm not a fan of having two copies, but I don't think it is worth moving to its own package
// to allow properties and ingest to both import without a cycle.
//
//goland:noinspection GoUnusedConst - Part of the API
const (
	// DFUnknown indicates the EncodingType is not set.
	DFUnknown DataFormat = properties.DFUnknown
	// AVRO indicates the source is encoded in Apache Avro format.
	AVRO DataFormat = properties.AVRO
	// ApacheAVRO indicates the source is encoded in Apache avro2json format.
	ApacheAVRO DataFormat = properties.ApacheAVRO
	// CSV indicates the source is encoded in comma seperated values.
	CSV DataFormat = properties.CSV
	// JSON indicates the source is encoded as one or more lines, each containing a record in Javascript Object Notation.
	JSON DataFormat = properties.JSON
	// MultiJSON indicates the source is encoded in JSON-Array of individual records in Javascript Object Notation. Optionally,
	//multiple documents can be concatenated.
	MultiJSON DataFormat = properties.MultiJSON
	// ORC indicates the source is encoded in Apache Optimized Row Columnar format.
	ORC DataFormat = properties.ORC
	// Parquet indicates the source is encoded in Apache Parquet format.
	Parquet DataFormat = properties.Parquet
	// PSV is pipe "|" separated values.
	PSV DataFormat = properties.PSV
	// Raw is a text file that has only a single string value.
	Raw DataFormat = properties.Raw
	// SCSV is a file containing semicolon ";" separated values.
	SCSV DataFormat = properties.SCSV
	// SOHSV is a file containing SOH-separated values(ASCII codepoint 1).
	SOHSV DataFormat = properties.SOHSV
	// SStream indicates the source is encoded as a Microsoft Cosmos Structured Streams format
	SStream DataFormat = properties.SStream
	// TSV is a file containing tab seperated values ("\t").
	TSV DataFormat = properties.TSV
	// TSVE is a file containing escaped-tab seperated values ("\t").
	TSVE DataFormat = properties.TSVE
	// TXT is a text file with lines ending with "\n".
	TXT DataFormat = properties.TXT
	// W3CLogFile indicates the source is encoded using W3C Extended Log File format
	W3CLogFile DataFormat = properties.W3CLogFile
	// SingleJSON indicates the source is a single JSON value -- newlines are regular whitespace.
	SingleJSON DataFormat = properties.SingleJSON
)

// InferFormatFromFileName looks at the file name and tries to discern what the file format is
func InferFormatFromFileName(fName string) DataFormat {
	return properties.DataFormatDiscovery(fName)
}

// IngestionMapping provides runtime mapping of the data being imported to the fields in the table.
// "ref" will be JSON encoded, so it can be any type that can be JSON marshalled. If you pass a string
// or []byte, it will be interpreted as already being JSON encoded.
// mappingKind can only be: CSV, JSON, AVRO, Parquet or ORC.
// The mappingKind parameter will also automatically set the FileFormat option.
func IngestionMapping(mapping interface{}, mappingKind DataFormat) FileOption {
	return option{
		run: func(p *properties.All) error {
			if !mappingKind.IsValidMappingKind() {
				return errors.ES(
					errors.OpUnknown,
					errors.KClientArgs,
					"IngestionMapping() option does not support EncodingType %v", mappingKind,
				).SetNoRetry()
			}

			var j string
			switch v := mapping.(type) {
			case string:
				j = v
			case []byte:
				j = string(v)
			default:
				b, err := json.Marshal(mapping)
				if err != nil {
					return errors.ES(
						errors.OpUnknown,
						errors.KClientArgs,
						"IngestMapping option was passed to an Ingest.Ingestion call that was not a string, []byte or could be JSON encoded: %s", err,
					).SetNoRetry()
				}
				j = string(b)
			}

			p.Ingestion.Additional.IngestionMapping = j
			p.Ingestion.Additional.IngestionMappingType = mappingKind
			p.Ingestion.Additional.Format = mappingKind

			return nil
		},
		clientScopes: QueuedClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "IngestionMapping",
	}
}

// IngestionMappingRef provides the name of a pre-created mapping for the data being imported to the fields in the table.
// mappingKind can only be: CSV, JSON, AVRO, Parquet or ORC.
// For more details, see: https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
// The mappingKind parameter will also automatically set the FileFormat option.
func IngestionMappingRef(refName string, mappingKind DataFormat) FileOption {
	return option{
		run: func(p *properties.All) error {
			if !mappingKind.IsValidMappingKind() {
				return errors.ES(errors.OpUnknown, errors.KClientArgs, "IngestionMappingRef() option does not support EncodingType %v", mappingKind).SetNoRetry()
			}
			p.Ingestion.Additional.IngestionMappingRef = refName
			p.Ingestion.Additional.IngestionMappingType = mappingKind
			p.Ingestion.Additional.Format = mappingKind
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "IngestionMappingRef",
	}
}

// DeleteSource deletes the source file from when it has been uploaded to Kusto.
func DeleteSource() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Source.DeleteLocalSource = true
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile,
		name:         "DeleteSource",
	}
}

// IgnoreSizeLimit ignores the size limit for data ingestion.
func IgnoreSizeLimit() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.IgnoreSizeLimit = true
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "IgnoreSizeLimit",
	}
}

// Tags are tags to be associated with the ingested ata.
func Tags(tags []string) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.Additional.Tags = tags
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "Tags",
	}
}

// IfNotExists provides a string value that, if specified, prevents ingestion from succeeding if the table already
// has data tagged with an ingest-by: tag with the same value. This ensures idempotent data ingestion.
// For more information see: https://docs.microsoft.com/en-us/azure/kusto/management/extents-overview#ingest-by-extent-tags
func IfNotExists(ingestByTag string) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.Additional.IngestIfNotExists = ingestByTag
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "IfNotExists",
	}
}

// ReportResultToTable option requests that the ingestion status will be tracked in an Azure table.
// Note using Table status reporting is not recommended for high capacity ingestions, as it could slow down the ingestion.
// In such cases, it's recommended to enable it temporarily for debugging failed ingestions.
func ReportResultToTable() FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.ReportLevel = properties.FailureAndSuccess
			p.Ingestion.ReportMethod = properties.ReportStatusToTable
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "ReportResultToTable",
	}
}

// SetCreationTime option allows the user to override the data creation time the retention policies are considered against
// If not set the data creation time is considered to be the time of ingestion
func SetCreationTime(t time.Time) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.Additional.CreationTime = t
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "SetCreationTime",
	}
}

// ValidationOption is an an option for validating the ingestion input data.
// These are defined as constants within this package.
type ValidationOption int8

//goland:noinspection GoUnusedConst - Part of the API
const (
	// VOUnknown indicates that a ValidationOption was not set.
	VOUnknown ValidationOption = 0
	// SameNumberOfFields indicates that all records ingested must have the same number of fields.
	SameNumberOfFields ValidationOption = 1
	// IgnoreNonDoubleQuotedFields indicates that fields that do not have double quotes should be ignored.
	IgnoreNonDoubleQuotedFields ValidationOption = 2
)

// ValidationImplication is a setting used to indicate what to do when a Validation Policy is violated.
// These are defined as constants within this package.
type ValidationImplication int8

//goland:noinspection GoUnusedConst - Part of the API
const (
	// FailIngestion indicates that any violation of the ValidationPolicy will cause the entire ingestion to fail.
	FailIngestion ValidationImplication = 0
	// IgnoreFailures indicates that failure of the ValidationPolicy will be ignored.
	IgnoreFailures ValidationImplication = 1
)

// ValPolicy sets a policy for validating data as it is sent for ingestion.
// For more information, see: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
type ValPolicy struct {
	// Options provides an option that will flag data that does not validate.
	Options ValidationOption `json:"ValidationOptions"`
	// Implications sets what to do when a policy option is violated.
	Implications ValidationImplication `json:"ValidationImplications"`
}

// ValidationPolicy uses a ValPolicy to set our ingestion data validation policy. If not set, no validation policy
// is used.
// For more information, see: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
func ValidationPolicy(policy ValPolicy) FileOption {
	return option{
		run: func(p *properties.All) error {
			b, err := json.Marshal(policy)
			if err != nil {
				return errors.ES(errors.OpUnknown, errors.KInternal, "bug: the ValPolicy provided would not JSON encode").SetNoRetry()
			}

			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Ingestion.Additional.ValidationPolicy = string(b)
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | ManagedClient,
		name:         "ValidationPolicy",
	}
}

// FileFormat can be used to indicate what type of encoding is supported for the file. This is only needed if
// the file extension is not present. A file like: "input.json.gz" or "input.json" does not need this option, while
// "input" would.
// If an ingestion mapping is specified, there is no need to specify the file format.
func FileFormat(et DataFormat) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.Additional.Format = et
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		name:         "FileFormat",
	}
}

// ClientRequestId is an identifier for the ingestion, that can later be queried.
func ClientRequestId(clientRequestId string) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Streaming.ClientRequestId = clientRequestId
			return nil
		},
		sourceScope:  FromFile | FromReader | FromBlob,
		clientScopes: StreamingClient | ManagedClient,
		name:         "ClientRequestId",
	}
}

// CompressionType sets the compression type of the data.
// Use this if the file name does not expose the compression type.
// This sets DontCompress to true for compressed data.
func CompressionType(compressionType ingestoptions.CompressionType) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Source.CompressionType = compressionType
			return nil
		},
		clientScopes: QueuedClient | StreamingClient | ManagedClient,
		sourceScope:  FromFile | FromReader,
		name:         "CompressionType",
	}
}

// RawDataSize is the uncompressed data size. Should be used to comunicate the file size to the service for efficient ingestion.
// Also used by managed client in the decision to use queued ingestion instead of streaming (if > 4mb)
func RawDataSize(size int64) FileOption {
	return option{
		run: func(p *properties.All) error {
			p.Ingestion.RawDataSize = size
			return nil
		},
		clientScopes: QueuedClient | ManagedClient,
		sourceScope:  FromFile | FromReader | FromBlob,
		name:         "RawDataSize",
	}
}
