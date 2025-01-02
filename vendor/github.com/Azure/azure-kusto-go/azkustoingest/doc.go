/*
Package azkustoingest provides a client for ingesting data into Azure Data Explorer (Kusto) clusters.

This package enables users to use different ingestion methods including queued, streaming, and managed ingestion from
various sources such as local files, Azure Blob Storage urls, streams, or any `io.Reader`.

To start using this package, create an instance of the Ingestor, passing in a connection string built using the
NewConnectionStringBuilder() function from the azkustodata package.

Example FromFile usage:

	kcsb := azkustodata.NewConnectionStringBuilder(`endpoint`).WithAadAppKey("clientID", "clientSecret", "tenentID")
	ingestor, err := azkustoingest.New(kcsb, azkustoingest.WithDefaultDatabase("database"), azkustoingest.WithDefaultTable("table"))

	if err != nil {
		// Handle error
	}

	defer ingestor.Close() // Always close the ingestor when done.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, err = ingestor.FromFile(ctx, "/path/to/file", azkustoingest.DeleteSource())

	... // Handle any errors and status

The package supports advanced features such as status reporting to Kusto tables, file deletion after ingestion, and handling of
retryable errors.

For complete documentation, please visit:
https://github.com/Azure/azure-kusto-go
https://pkg.go.dev/github.com/Azure/azure-kusto-go/azkustoingest
*/
package azkustoingest
