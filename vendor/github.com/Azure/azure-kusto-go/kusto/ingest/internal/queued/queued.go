// Package filesystem provides a client with the ability to import data into Kusto via a variety of fileystems
// such as local storage or blobstore.
package queued

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/ingestoptions"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/utils"

	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-storage-queue-go/azqueue"
	"github.com/google/uuid"
)

const (
	_1MiB = 1024 * 1024

	// The numbers below are magic numbers. They were derived from doing Azure to Azure tests of azcopy for various file sizes
	// to prove that changes weren't going to make azcopy slower. It was found that multiplying azcopy's concurrency by 10x (to 50)
	// made a 5x improvement in speed. We don't have any numbers from the service side to give us numbers we should use, so this
	// is our best guess from observation. DO NOT CHANGE UNLESS YOU KNOW BETTER.

	BlockSize             = 8 * _1MiB
	Concurrency           = 50
	StorageMaxRetryPolicy = 3
)

// Queued provides methods for taking data from various sources and ingesting it into Kusto using queued ingestion.
type Queued interface {
	io.Closer
	Local(ctx context.Context, from string, props properties.All) error
	Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error)
	Blob(ctx context.Context, from string, fileSize int64, props properties.All) error
}

// uploadStream provides a type that mimics `azblob.UploadStream` to allow fakes for testing.
type uploadStream func(context.Context, io.Reader, *azblob.Client, string, string, *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error)

// uploadBlob provides a type that mimics `azblob.UploadFile` to allow fakes for test
type uploadBlob func(context.Context, *os.File, *azblob.Client, string, string, *azblob.UploadFileOptions) (azblob.UploadFileResponse, error)

// Ingestion provides methods for taking data from a filesystem of some type and ingesting it into Kusto.
// This object is scoped for a single database and table.
type Ingestion struct {
	http  *http.Client
	db    string
	table string
	mgr   *resources.Manager

	uploadStream uploadStream
	uploadBlob   uploadBlob

	bufferSize int
	maxBuffers int
}

// Option is an optional argument to New().
type Option func(s *Ingestion)

// WithStaticBuffer sets a static buffer with a buffer size and max amount of buffers for uploading blobs to kusto.
func WithStaticBuffer(bufferSize int, maxBuffers int) Option {
	return func(s *Ingestion) {
		s.bufferSize = bufferSize
		s.maxBuffers = maxBuffers
	}
}

// New is the constructor for Ingestion.
func New(db, table string, mgr *resources.Manager, http *http.Client, options ...Option) (*Ingestion, error) {
	i := &Ingestion{
		db:    db,
		table: table,
		mgr:   mgr,
		http:  http,
		uploadStream: func(ctx context.Context, reader io.Reader, client *azblob.Client, container, blob string,
			options *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error) {
			return client.UploadStream(ctx, container, blob, reader, options)
		},
		uploadBlob: func(ctx context.Context, file *os.File, client *azblob.Client, container, blob string,
			options *azblob.UploadFileOptions) (azblob.UploadFileResponse, error) {
			return client.UploadFile(ctx, container, blob, file, options)
		},
	}

	for _, opt := range options {
		opt(i)
	}

	return i, nil
}

// Local ingests a local file into Kusto.
func (i *Ingestion) Local(ctx context.Context, from string, props properties.All) error {
	containers, err := i.mgr.GetRankedStorageContainers()
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return errors.ES(
			errors.OpFileIngest,
			errors.KBlobstore,
			"no Blob Storage container resources are defined, there is no container to upload to",
		).SetNoRetry()
	}

	queues, err := i.mgr.GetRankedStorageQueues()
	if err != nil {
		return err
	}

	// We want to check the queue size here so we don't upload a file and then find we don't have a Kusto queue to stick
	// it in. If we don't have a container, that is handled by containerQueue().
	if len(queues) == 0 {
		return errors.ES(errors.OpFileIngest, errors.KBlobstore, "no Kusto queue resources are defined, there is no queue to upload to").SetNoRetry()
	}

	// Go over all the containers and try to upload the file to each one. If we succeed, we are done.
	for attempts, containerUri := range containers {
		if attempts >= StorageMaxRetryPolicy {
			return errors.ES(errors.OpFileIngest, errors.KBlobstore, "max retry policy reached").SetNoRetry()
		}

		client, containerName, err := i.upstreamContainer(containerUri)
		if err != nil {
			i.mgr.ReportStorageResourceResult(containerUri.Account(), false)
			continue
		}

		blobURL, size, err := i.localToBlob(ctx, from, client, containerName, &props)
		if err == nil {
			i.mgr.ReportStorageResourceResult(containerUri.Account(), true)
			return i.Blob(ctx, blobURL, size, props)
		}

		// check if the error is retryable
		if errors.Retry(err) {
			i.mgr.ReportStorageResourceResult(containerUri.Account(), false)
			continue
		} else {
			return err
		}
	}

	return errors.ES(errors.OpFileIngest, errors.KBlobstore, "could not upload file to any container")
}

// Reader uploads a file via an io.Reader.
// If the function succeeds, it returns the path of the created blob.
func (i *Ingestion) Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error) {
	containers, err := i.mgr.GetRankedStorageContainers()
	if err != nil {
		return "", err
	}

	if len(containers) == 0 {
		return "", errors.ES(
			errors.OpFileIngest,
			errors.KBlobstore,
			"no Blob Storage container resources are defined, there is no container to upload to",
		).SetNoRetry()
	}

	queues, err := i.mgr.GetRankedStorageQueues()
	if err != nil {
		return "", err
	}

	// We want to check the queue size here so so we don't upload a file and then find we don't have a Kusto queue to stick
	// it in. If we don't have a container, that is handled by containerQueue().
	if len(queues) == 0 {
		return "", errors.ES(errors.OpFileIngest, errors.KBlobstore, "no Kusto queue resources are defined, there is no queue to upload to").SetNoRetry()
	}

	compression := utils.CompressionDiscovery(props.Source.OriginalSource)
	shouldCompress := ShouldCompress(&props, compression)
	blobName := GenBlobName(i.db, i.table, nower(), filepath.Base(uuid.New().String()), filepath.Base(props.Source.OriginalSource), compression, shouldCompress, props.Ingestion.Additional.Format.String())

	size := int64(0)

	if shouldCompress {
		reader = gzip.Compress(reader)
	}

	// Go over all the containers and try to upload the file to each one. If we succeed, we are done.
	for attempts, containerUri := range containers {
		if attempts >= StorageMaxRetryPolicy {
			return "", errors.ES(errors.OpFileIngest, errors.KBlobstore, "max retry policy reached").SetNoRetry()
		}

		client, containerName, err := i.upstreamContainer(containerUri)
		if err != nil {
			i.mgr.ReportStorageResourceResult(containerUri.Account(), false)
			continue
		}

		_, err = i.uploadStream(
			ctx,
			reader,
			client,
			containerName,
			blobName,
			&azblob.UploadStreamOptions{BlockSize: int64(i.bufferSize), Concurrency: i.maxBuffers},
		)

		if err != nil {
			i.mgr.ReportStorageResourceResult(containerUri.Account(), false)
			continue
		}

		i.mgr.ReportStorageResourceResult(containerUri.Account(), true)
		if gz, ok := reader.(*gzip.Streamer); ok {
			size = gz.InputSize()
		}
		err = i.Blob(ctx, fullUrl(client, containerName, blobName), size, props)
		return blobName, err
	}

	return blobName, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage")
}

// Blob ingests a file from Azure Blob Storage into Kusto.
func (i *Ingestion) Blob(ctx context.Context, from string, fileSize int64, props properties.All) error {
	// To learn more about ingestion properties, go to:
	// https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/#ingestion-properties
	// To learn more about ingestion methods go to:
	// https://docs.microsoft.com/en-us/azure/data-explorer/ingest-data-overview#ingestion-methods

	props.Ingestion.BlobPath = from
	if fileSize != 0 {
		props.Ingestion.RawDataSize = fileSize
	}

	props.Ingestion.RetainBlobOnSuccess = !props.Source.DeleteLocalSource

	err := CompleteFormatFromFileName(&props, from)
	if err != nil {
		return err
	}

	j, err := props.Ingestion.MarshalJSONString()
	if err != nil {
		return errors.ES(errors.OpFileIngest, errors.KInternal, "could not marshal the ingestion blob info: %s", err).SetNoRetry()
	}

	queueResources, err := i.mgr.GetRankedStorageQueues()
	if err != nil {
		return err
	}

	// Go over all the queues and try to upload the file to each one. If we succeed, we are done.
	for attempts, queueUri := range queueResources {
		if attempts >= StorageMaxRetryPolicy {
			return errors.ES(errors.OpFileIngest, errors.KBlobstore, "max retry policy reached").SetNoRetry()
		}
		queueClient := i.upstreamQueue(queueUri)
		if _, err := queueClient.Enqueue(ctx, j, 0, 0); err != nil {
			i.mgr.ReportStorageResourceResult(queueUri.Account(), false)
			continue
		} else {
			i.mgr.ReportStorageResourceResult(queueUri.Account(), true)
			return props.ApplyDeleteLocalSourceOption()
		}
	}

	return errors.ES(errors.OpFileIngest, errors.KBlobstore, "could not upload file to any queue")
}

func CompleteFormatFromFileName(props *properties.All, from string) error {
	// If they did not tell us how the file was encoded, try to discover it from the file extension.
	if props.Ingestion.Additional.Format != properties.DFUnknown {
		return nil
	}

	et := properties.DataFormatDiscovery(from)
	if et == properties.DFUnknown {
		// If we can't figure out the file type, default to CSV.
		et = properties.CSV
	}
	props.Ingestion.Additional.Format = et

	return nil
}

func (i *Ingestion) upstreamContainer(resourceUri *resources.URI) (*azblob.Client, string, error) {
	storageUrl := resourceUri.URL()
	serviceURL := fmt.Sprintf("%s://%s?%s", storageUrl.Scheme, storageUrl.Host, resourceUri.SAS().Encode())

	client, err := azblob.NewClientWithNoCredential(serviceURL, &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: i.http,
		},
	})

	if err != nil {
		return nil, "", errors.E(errors.OpFileIngest, errors.KBlobstore, err)
	}

	return client, resourceUri.ObjectName(), nil
}

func (i *Ingestion) upstreamQueue(resourceUri *resources.URI) azqueue.MessagesURL {
	queueUrl := resourceUri.URL()
	service, _ := url.Parse(fmt.Sprintf("%s://%s?%s", queueUrl.Scheme, queueUrl.Host, resourceUri.SAS().Encode()))

	p := createPipeline(i.http)

	return azqueue.NewServiceURL(*service, p).NewQueueURL(resourceUri.ObjectName()).NewMessagesURL()
}

func createPipeline(http *http.Client) pipeline.Pipeline {
	// This is a lot of boilerplate, but all it does is setting the http client to be our own.
	return pipeline.NewPipeline([]pipeline.Factory{pipeline.MethodFactoryMarker()}, pipeline.Options{
		HTTPSender: pipeline.FactoryFunc(func(next pipeline.Policy, po *pipeline.PolicyOptions) pipeline.PolicyFunc {
			return func(ctx context.Context, request pipeline.Request) (pipeline.Response, error) {
				r, err := http.Do(request.WithContext(ctx))
				if err != nil {
					err = pipeline.NewError(err, "HTTP request failed")
				}
				return pipeline.NewHTTPResponse(r), err
			}
		}),
	})
}

var nower = time.Now

// localToBlob copies from a local to an Azure Blobstore blob. It returns the URL of the Blob, the local file info and an
// error if there was one.
func (i *Ingestion) localToBlob(ctx context.Context, from string, client *azblob.Client, container string, props *properties.All) (string, int64, error) {
	compression := utils.CompressionDiscovery(from)
	shouldCompress := ShouldCompress(props, compression)
	blobName := GenBlobName(i.db, i.table, nower(), filepath.Base(uuid.New().String()), filepath.Base(from), compression, shouldCompress, props.Ingestion.Additional.Format.String())

	file, err := os.Open(from)
	if err != nil {
		return "", 0, errors.ES(
			errors.OpFileIngest,
			errors.KLocalFileSystem,
			"problem retrieving source file %q: %s", from, err,
		).SetNoRetry()
	}

	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", 0, errors.ES(
			errors.OpFileIngest,
			errors.KLocalFileSystem,
			"could not Stat the file(%s): %s", from, err,
		).SetNoRetry()
	}

	if shouldCompress {
		gstream := gzip.New()
		gstream.Reset(file)

		_, err = i.uploadStream(
			ctx,
			gstream,
			client,
			container,
			blobName,
			&azblob.UploadStreamOptions{BlockSize: int64(i.bufferSize), Concurrency: i.maxBuffers},
		)

		if err != nil {
			return "", 0, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage: %s", err)
		}
		return fullUrl(client, container, blobName), gstream.InputSize(), nil
	}

	// The high-level API UploadFileToBlockBlob function uploads blocks in parallel for optimal performance, and can handle large files as well.
	// This function calls StageBlock/CommitBlockList for files larger 256 MBs, and calls Upload for any file smaller
	_, err = i.uploadBlob(
		ctx,
		file,
		client,
		container,
		blobName,
		&azblob.UploadFileOptions{
			BlockSize:   BlockSize,
			Concurrency: Concurrency,
		},
	)

	if err != nil {
		return "", 0, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage: %s", err)
	}

	return fullUrl(client, container, blobName), stat.Size(), nil
}

func GenBlobName(databaseName string, tableName string, time time.Time, guid string, fileName string, compressionFileExtension ingestoptions.CompressionType, shouldCompress bool, dataFormat string) string {
	extension := "gz"
	if !shouldCompress {
		if compressionFileExtension == ingestoptions.CTNone {
			extension = dataFormat
		} else {
			extension = compressionFileExtension.String()
		}

		extension = dataFormat
	}

	blobName := fmt.Sprintf("%s_%s_%s_%s_%s.%s", databaseName, tableName, time, guid, fileName, extension)

	return blobName
}

// Do not compress if user specified in DontCompress or CompressionType,
// if the file extension shows compression, or if the format is binary.
func ShouldCompress(props *properties.All, compressionFileExtension ingestoptions.CompressionType) bool {
	if props.Source.DontCompress {
		return false
	}

	if props.Source.CompressionType != ingestoptions.CTUnknown {
		if props.Source.CompressionType != ingestoptions.CTNone {
			return false
		}
	} else {
		if compressionFileExtension != ingestoptions.CTUnknown && compressionFileExtension != ingestoptions.CTNone {
			return false
		}
	}

	return props.Ingestion.Additional.Format.ShouldCompress()
}

// This allows mocking the stat func later on
var statFunc = os.Stat

// IsLocalPath detects whether a path points to a file system accessiable file
// If this file requires another protocol http protocol it will return false
// If the file requires another protocol(ftp, https, etc) it will return an error
func IsLocalPath(s string) (bool, error) {
	u, err := url.Parse(s)
	if err == nil {
		switch u.Scheme {
		// With this we know it SHOULD be a blobstore path.  It might not be, but I think that is a fine assumption to make.
		case "http", "https":
			return false, nil
		}
	}

	// By this point, we know its not blobstore, so it needs to be something that gets resolved to a file.
	// So we are going to Stat() the file and see if it exists and is not a directory.
	// In your tests, this would fail "file://" which we don't support.  Also, because of this method, your tests
	// are going to be broken.   Again, fileystems, blah....
	stat, err := statFunc(s)
	if err != nil {
		return false, fmt.Errorf("It is not a valid local file path (could not stat file) and not a valid blob path")
	}

	if stat.IsDir() {
		return false, fmt.Errorf("path is a local directory and not a valid file")
	}

	return true, nil
}

func fullUrl(client *azblob.Client, container string, blob string) string {
	parseURL, err := azblob.ParseURL(client.URL())
	if err != nil {
		return ""
	}
	parseURL.ContainerName = container
	parseURL.BlobName = blob

	return parseURL.String()
}

func (i *Ingestion) Close() error {
	i.mgr.Close()
	return nil
}
