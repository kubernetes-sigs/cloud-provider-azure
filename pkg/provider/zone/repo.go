/*
Copyright 2024 The Kubernetes Authors.

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

package zone

// Generate mocks for the repository interface
//go:generate mockgen -destination=./mock_repo.go -package=zone -copyright_file ../../../hack/boilerplate/boilerplate.generatego.txt -source=repo.go Repository

import (
	"context"
	"strings"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/providerclient"
	fnutil "sigs.k8s.io/cloud-provider-azure/pkg/util/collectionutil"
)

const (
	// networkProviderNamespace is the ARM provider whose zoneMappings say which zones a
	// Microsoft.Network resource may be created in.
	networkProviderNamespace = "Microsoft.Network"

	// PublicIPResourceType is the resource type behind a public IP address.
	PublicIPResourceType = "publicIPAddresses"
)

type Repository interface {
	// ListZones returns the zones the subscription supports per region for virtual machines.
	ListZones(ctx context.Context) (map[string][]string, error)
	// ListNetworkResourceZones returns the zones supported per region by one
	// Microsoft.Network resource type. ARM validates a new resource against the zone
	// mappings of its own resource type, which can be narrower than the virtual-machine
	// ones returned by ListZones.
	ListNetworkResourceZones(ctx context.Context, resourceType string) (map[string][]string, error)
}

type repo struct {
	client providerclient.Interface
}

func NewRepo(client providerclient.Interface) (Repository, error) {
	return &repo{client: client}, nil
}

func (r *repo) ListZones(ctx context.Context) (map[string][]string, error) {
	zones, err := r.client.GetVirtualMachineSupportedZones(ctx)
	if err != nil {
		return nil, err
	}

	var rv = make(map[string][]string, len(zones))
	for region, z := range zones {
		rv[region] = fnutil.Map(func(s *string) string { return *s }, z)
	}

	return rv, nil
}

func (r *repo) ListNetworkResourceZones(ctx context.Context, resourceType string) (map[string][]string, error) {
	provider, err := r.client.GetProvider(ctx, networkProviderNamespace)
	if err != nil {
		return nil, err
	}

	rv := make(map[string][]string)
	for _, declared := range provider.ResourceTypes {
		if declared == nil || declared.ResourceType == nil ||
			!strings.EqualFold(*declared.ResourceType, resourceType) {
			continue
		}
		for _, zoneMapping := range declared.ZoneMappings {
			if zoneMapping == nil || zoneMapping.Location == nil {
				continue
			}
			// Key the regions the way GetVirtualMachineSupportedZones does so that both maps
			// can be looked up with the same location string.
			region := strings.ToLower(strings.ReplaceAll(*zoneMapping.Location, " ", ""))
			rv[region] = fnutil.Map(func(zone *string) string { return *zone }, zoneMapping.Zones)
		}
	}

	return rv, nil
}
