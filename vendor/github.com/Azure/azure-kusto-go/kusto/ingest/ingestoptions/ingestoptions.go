package ingestoptions

// CompressionType is a file's compression type.
type CompressionType int8

// String implements fmt.Stringer.
func (c CompressionType) String() string {
	switch c {
	case GZIP:
		return "gzip"
	case ZIP:
		return "zip"
	}
	return "unknown compression type"
}

//goland:noinspection GoUnusedConst - Part of the API
const (
	// CTUnknown indicates that that the compression type was unset.
	CTUnknown CompressionType = 0
	// CTNone indicates that the file was not compressed.
	CTNone CompressionType = 1
	// GZIP indicates that the file is GZIP compressed.
	GZIP CompressionType = 2
	// ZIP indicates that the file is ZIP compressed.
	ZIP CompressionType = 3
)
