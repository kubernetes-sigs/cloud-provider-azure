package ingest

import (
	"strings"
)

const ingestPrefix = "ingest-"

func removeIngestPrefix(s string) string {
	return strings.Replace(s, ingestPrefix, "", 1)
}
