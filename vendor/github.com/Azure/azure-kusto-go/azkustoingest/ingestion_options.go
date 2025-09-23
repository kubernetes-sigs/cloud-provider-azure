package azkustoingest

import (
	"github.com/Azure/azure-kusto-go/azkustodata"
	"net"
	"net/http"
	"strings"
)

// Option is an optional argument to New().
type Option func(s *Ingestion)

// WithStaticBuffer configures the ingest client to upload data to Kusto using a set of one or more static memory buffers with a fixed size.
// Only relevant for Queued and Managed ingestion.
func WithStaticBuffer(bufferSize int, maxBuffers int) Option {
	return func(s *Ingestion) {
		s.bufferSize = bufferSize
		s.maxBuffers = maxBuffers
	}
}

// WithDefaultDatabase configures the ingest client to use the given database name as the default database for all ingest operations.
func WithDefaultDatabase(db string) Option {
	return func(s *Ingestion) {
		s.db = db
	}
}

// WithDefaultTable configures the ingest client to use the given table name as the default table for all ingest operations.
func WithDefaultTable(table string) Option {
	return func(s *Ingestion) {
		s.table = table
	}
}

// WithoutEndpointCorrection disables the automatic correction of the Kusto cluster address.
// The address will be used as-is, without adding or removing the "ingest-" prefix.
func WithoutEndpointCorrection() Option {
	return func(s *Ingestion) {
		s.withoutEndpointCorrection = true
	}
}

// WithCustomIngestConnectionString is relevant to Managed ingestion client only.
// It configures the ingest client using a custom connection string, as opposed to one derived from the streaming client.
// This option implies WithoutEndpointCorrection().
func WithCustomIngestConnectionString(kcsb *azkustodata.ConnectionStringBuilder) Option {
	return func(s *Ingestion) {
		s.withoutEndpointCorrection = true
		s.customIngestConnectionString = kcsb
	}
}

// WithHttpClient configures the ingest client to use a custom HTTP client.
// This allows for customization of the HTTP transport, including adding instrumentation like OpenTelemetry.
func WithHttpClient(client *http.Client) Option {
	return func(s *Ingestion) {
		s.httpClient = client
	}
}

func getOptions(options []Option) *Ingestion {
	s := &Ingestion{}
	for _, o := range options {
		o(s)
	}
	return s
}

const domainPrefix = "://"
const ingestPrefix = "ingest-"

func removeIngestPrefix(s string) string {
	if isReservedHostname(s) {
		return s
	}

	return strings.Replace(s, ingestPrefix, "", 1)
}

func addIngestPrefix(s string) string {
	if isReservedHostname(s) {
		return s
	}
	if strings.Contains(s, ingestPrefix) {
		return s
	}

	if strings.Contains(s, domainPrefix) {
		return strings.Replace(s, domainPrefix, domainPrefix+ingestPrefix, 1)
	} else {
		return ingestPrefix + s
	}
}

func isReservedHostname(host string) bool {
	if strings.Contains(host, domainPrefix) {
		host = strings.Split(host, domainPrefix)[1]
	}

	// Check if host is an IP address
	if ip := net.ParseIP(host); ip != nil {
		return true
	}

	// Check if host is "localhost"
	if strings.ToLower(host) == "localhost" {
		return true
	}

	// Check if host is "onebox.dev.kusto.windows.net"
	if host == "onebox.dev.kusto.windows.net" {
		return true
	}

	// If none of the conditions match, return false
	return false
}
