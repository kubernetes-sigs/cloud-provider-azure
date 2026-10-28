/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package recording

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
)

// DeletePrivateDNSZone waits for previously deleted links to stop blocking zone cleanup.
func DeletePrivateDNSZone(ctx context.Context, client *armprivatedns.PrivateZonesClient, resourceGroupName, zoneName string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	for {
		poller, err := client.BeginDelete(ctx, resourceGroupName, zoneName, nil)
		if err == nil {
			_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: time.Second})
		}
		if err == nil {
			return nil
		}
		var responseError *azcore.ResponseError
		if !errors.As(err, &responseError) || responseError.StatusCode != http.StatusConflict || responseError.ErrorCode != "CannotDeleteResource" {
			return fmt.Errorf("delete private DNS zone %q: %w", zoneName, err)
		}

		// A completed link deletion can remain visible to the parent zone temporarily.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("delete private DNS zone %q: %w (last response: %w)", zoneName, ctx.Err(), err)
		case <-timer.C:
		}
	}
}
