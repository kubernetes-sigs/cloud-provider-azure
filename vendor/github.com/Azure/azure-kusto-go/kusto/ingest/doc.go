/*
Package ingest provides data ingestion from various external sources into Kusto.

For more information on Kusto Data Ingestion, please see: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/


Create a client

Creating a client simply requires a *kusto.Client, the name of the database and the name of the table to be ingested into.

	in, err := ingest.New(kustoClient, "database", "table")
	if err != nil {
		panic("add error handling")
	}


Ingestion from a local file

Ingesting a local file requires simply passing the path to the file to be ingested:

	if _, err := in.FromFile(ctx, "/path/to/a/local/file"); err != nil {
		panic("add error handling")
	}

FromFile() will accept Unix path names on Unix platforms and Windows path names on Windows platforms.
The file will not be deleted after upload (there is an option that will allow that though).


Ingestion from an Azure Blob Storage file

This package will also accept ingestion from an Azure Blob Storage file:

	if _, err := in.FromFile(ctx, "https://myaccount.blob.core.windows.net/$root/myblob"); err != nil {
		panic("add error handling")
	}

This will ingest a file from Azure Blob Storage. We only support https:// paths and your domain name may differ than what is here.

Ingestion from an io.Reader

Sometimes you want to ingest a stream of data that you have in memory without writing to disk.  You can do this simply by chunking the
data via an io.Reader.

	r, w := io.Pipe()

	enc := json.NewEncoder(w)
	go func() {
		defer w.Close()
		for _, data := range dataSet {
			if err := enc.Encode(data); err != nil {
				panic("add error handling")
			}
		}
	}()

	if _, err := in.FromReader(ctx, r); err != nil {
		panic("add error handling")
	}

It is important to remember that FromReader() will terminate when it receives an io.EOF from the io.Reader.  Use io.Readers that won't
return io.EOF until the io.Writer is closed (such as io.Pipe).

Ingestion from a Stream

Instestion from a stream commits blocks of fully formed data encodes (JSON, AVRO, ...) into Kusto:

	if err := in.Stream(ctx , jsonEncodedData, ingest.JSON, "mappingName"); err != nil {
		panic("add error handling")
	}

Ingestion with Status Reporting

You can use Kusto Go SDK to get table-based status reporting of ingestion operations.
Ingestion commands run using FromFile() and FromReader() return an error and a channel that can be waited upon for a final status.
If the error is not nil, the operation has failed locally.
If the error is nil and Table Status Reporting option was used, the SDK user can wait on the channel for a success (nil) or failure (Error) status.

Note!
This feature is not suitable for users running ingestion at high rates, and may slow down the ingestion operation.

	status, err := ingestor.FromFile(ctx, "/path/to/file", ingest.ReportResultToTable())
	if err != nil {
		// The ingestion command failed to be sent, Do something
	}

	err = <-status.Wait(ctx)
	if err != nil {
		// the operation complete with an error
		if ingest.IsRetryable(err) {
			// Handle retries
		} else {
			// inspect the failure
			// statusCode, _ := ingest.GetIngestionStatus(err)
			// failureStatus, _ := ingest.GetIngestionFailureStatus(err)
		}
	}
*/
package ingest
