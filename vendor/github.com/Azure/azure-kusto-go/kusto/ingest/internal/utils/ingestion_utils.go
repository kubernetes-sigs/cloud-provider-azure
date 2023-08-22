// Package resources contains objects that are used to gather information about Kusto resources that are
// used during various ingestion methods.
package utils

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
)

const EstimatedCompressionFactor = 11

func FetchBlobSize(fPath string, ctx context.Context, client *http.Client) (size int64, err error) {
	if !strings.Contains(fPath, ".blob.") || strings.Contains(fPath, "Managed_Identity=") || strings.Contains(fPath, "Token=") {
		return 0, nil
	}

	parsed, err := resources.Parse(fPath)
	if err != nil {
		return 0, err
	}

	var blobClient *azblob.Client = nil
	var objectNameSplit []string

	if len(parsed.SAS()) == 0 {
		objectParts := strings.Split(parsed.ObjectName(), ";")
		if len(objectParts) == 2 {
			cred, err := service.NewSharedKeyCredential(parsed.Account(), objectParts[1])
			if err != nil {
				return 0, err
			}

			serviceUrl := fmt.Sprintf("%s://%s", parsed.URL().Scheme, parsed.URL().Host)
			blobClient, err = azblob.NewClientWithSharedKeyCredential(serviceUrl, cred, &azblob.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Transport: client,
				},
			})
			if err != nil {
				return 0, err
			}

			objectNameSplit = strings.SplitN(objectParts[0], "/", 2)
		}
	}

	if blobClient == nil {
		serviceUrl := fmt.Sprintf("%s://%s?%s", parsed.URL().Scheme, parsed.URL().Host, parsed.SAS().Encode())
		blobClient, err = azblob.NewClientWithNoCredential(serviceUrl, &azblob.ClientOptions{
			ClientOptions: azcore.ClientOptions{
				Transport: client,
			},
		})
		if err != nil {
			return 0, err
		}

		objectNameSplit = strings.SplitN(parsed.ObjectName(), "/", 2)
	}

	blobCli := blobClient.ServiceClient().NewContainerClient(objectNameSplit[0]).NewBlobClient(objectNameSplit[1])
	properties, err := blobCli.GetProperties(ctx, &blob.GetPropertiesOptions{})
	if err != nil {
		return 0, err
	}

	return *properties.ContentLength, nil
}

func EstimateRawDataSize(compression properties.CompressionType, fileSize int64) int64 {
	switch compression {
	case properties.GZIP:
	case properties.ZIP:
		return fileSize * EstimatedCompressionFactor
	}

	return fileSize
}

// CompressionDiscovery looks at the file extension. If it is one we support, we return that
// CompressionType that represents that value. Otherwise we return CTNone to indicate that the
// file should not be compressed.
func CompressionDiscovery(fName string) properties.CompressionType {
	var ext string
	if strings.HasPrefix(strings.ToLower(fName), "http") {
		ext = strings.ToLower(filepath.Ext(path.Base(fName)))
	} else {
		ext = strings.ToLower(filepath.Ext(fName))
	}

	switch ext {
	case ".gz":
		return properties.GZIP
	case ".zip":
		return properties.ZIP
	}
	return properties.CTNone
}
