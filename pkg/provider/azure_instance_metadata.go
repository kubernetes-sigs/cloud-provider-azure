/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"k8s.io/apimachinery/pkg/util/validation"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
)

// NetworkMetadata contains metadata about an instance's network
type NetworkMetadata struct {
	Interface []*NetworkInterface `json:"interface"`
}

// NetworkInterface represents an instances network interface.
type NetworkInterface struct {
	IPV4 NetworkData `json:"ipv4"`
	IPV6 NetworkData `json:"ipv6"`
	MAC  string      `json:"macAddress"`
}

// NetworkData contains IP information for a armnetwork.
type NetworkData struct {
	IPAddress []IPAddress `json:"ipAddress"`
	Subnet    []Subnet    `json:"subnet"`
}

// IPAddress represents IP address information.
type IPAddress struct {
	PrivateIP string `json:"privateIpAddress"`
	PublicIP  string `json:"publicIpAddress"`
}

// Subnet represents subnet information.
type Subnet struct {
	Address string `json:"address"`
	Prefix  string `json:"prefix"`
}

// Tag represents a key-value tag in IMDS.
type Tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ComputeMetadata represents compute information
type ComputeMetadata struct {
	Environment            string `json:"azEnvironment,omitempty"`
	SKU                    string `json:"SKU,omitempty"`
	Name                   string `json:"name,omitempty"`
	Zone                   string `json:"zone,omitempty"`
	VMSize                 string `json:"vmSize,omitempty"`
	OSType                 string `json:"osType,omitempty"`
	Location               string `json:"location,omitempty"`
	FaultDomain            string `json:"platformFaultDomain,omitempty"`
	PlatformSubFaultDomain string `json:"platformSubFaultDomain,omitempty"`
	UpdateDomain           string `json:"platformUpdateDomain,omitempty"`
	ResourceGroup          string `json:"resourceGroupName,omitempty"`
	VMScaleSetName         string `json:"vmScaleSetName,omitempty"`
	SubscriptionID         string `json:"subscriptionId,omitempty"`
	ResourceID             string `json:"resourceId,omitempty"`
	InterconnectGroupID    string `json:"interconnectGroupId,omitempty"`
	InterconnectSubgroupID string `json:"interconnectSubgroupId,omitempty"`
	TagsList               []Tag  `json:"tagsList,omitempty"`
}

// UnmarshalJSON rejects non-string M2 IDs, including null, rather than selecting M1.
func (compute *ComputeMetadata) UnmarshalJSON(data []byte) error {
	type computeMetadata ComputeMetadata
	decoded := struct {
		*computeMetadata
		InterconnectGroupID    json.RawMessage `json:"interconnectGroupId"`
		InterconnectSubgroupID json.RawMessage `json:"interconnectSubgroupId"`
	}{computeMetadata: (*computeMetadata)(compute)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
		dest *string
	}{
		{"interconnectGroupId", decoded.InterconnectGroupID, &compute.InterconnectGroupID},
		{"interconnectSubgroupId", decoded.InterconnectSubgroupID, &compute.InterconnectSubgroupID},
	} {
		if len(field.raw) == 0 {
			continue
		}
		// encoding/json otherwise accepts null when decoding a Go string.
		if string(field.raw) == "null" {
			return fmt.Errorf("compute metadata field %q must be a string", field.name)
		}
		if err := json.Unmarshal(field.raw, field.dest); err != nil {
			return fmt.Errorf("decode compute metadata field %q: %w", field.name, err)
		}
	}
	return nil
}

// InstanceMetadata represents instance information.
type InstanceMetadata struct {
	Compute *ComputeMetadata `json:"compute,omitempty"`
	Network *NetworkMetadata `json:"network,omitempty"`
}

// PublicIPMetadata represents the public IP metadata.
type PublicIPMetadata struct {
	FrontendIPAddress string `json:"frontendIpAddress,omitempty"`
	PrivateIPAddress  string `json:"privateIpAddress,omitempty"`
}

// LoadbalancerProfile represents load balancer profile in IMDS.
type LoadbalancerProfile struct {
	PublicIPAddresses []PublicIPMetadata `json:"publicIpAddresses,omitempty"`
}

// LoadBalancerMetadata represents load balancer metadata.
type LoadBalancerMetadata struct {
	LoadBalancer *LoadbalancerProfile `json:"loadbalancer,omitempty"`
}

// InstanceMetadataService knows how to query the Azure instance metadata server.
type InstanceMetadataService struct {
	imdsServer string
	imsCache   azcache.Resource
}

// NewInstanceMetadataService creates an instance of the InstanceMetadataService accessor object.
func NewInstanceMetadataService(imdsServer string) (*InstanceMetadataService, error) {
	ims := &InstanceMetadataService{
		imdsServer: imdsServer,
	}

	imsCache, err := azcache.NewTimedCache(consts.MetadataCacheTTL, ims.getMetadata, false)
	if err != nil {
		return nil, err
	}

	ims.imsCache = imsCache
	return ims, nil
}

// fillNetInterfacePublicIPs finds PIPs from imds load balancer and fills them into net interface config.
func fillNetInterfacePublicIPs(publicIPs []PublicIPMetadata, netInterface *NetworkInterface) {
	// IPv6 IPs from imds load balancer are wrapped by brackets while those from imds are not.
	trimIP := func(ip string) string {
		return strings.Trim(strings.Trim(ip, "["), "]")
	}

	if len(netInterface.IPV4.IPAddress) > 0 && len(netInterface.IPV4.IPAddress[0].PrivateIP) > 0 {
		for _, pip := range publicIPs {
			if pip.PrivateIPAddress == netInterface.IPV4.IPAddress[0].PrivateIP {
				netInterface.IPV4.IPAddress[0].PublicIP = pip.FrontendIPAddress
				break
			}
		}
	}
	if len(netInterface.IPV6.IPAddress) > 0 && len(netInterface.IPV6.IPAddress[0].PrivateIP) > 0 {
		for _, pip := range publicIPs {
			privateIP := trimIP(pip.PrivateIPAddress)
			frontendIP := trimIP(pip.FrontendIPAddress)
			if privateIP == netInterface.IPV6.IPAddress[0].PrivateIP {
				netInterface.IPV6.IPAddress[0].PublicIP = frontendIP
				break
			}
		}
	}
}

func (ims *InstanceMetadataService) getMetadata(ctx context.Context, key string) (interface{}, error) {
	logger := log.FromContextOrBackground(ctx).WithName("getMetadata")
	instanceMetadata, err := ims.getInstanceMetadata(key)
	if err != nil {
		return nil, err
	}

	if instanceMetadata.Network != nil && len(instanceMetadata.Network.Interface) > 0 {
		netInterface := instanceMetadata.Network.Interface[0]
		if (len(netInterface.IPV4.IPAddress) > 0 && len(netInterface.IPV4.IPAddress[0].PublicIP) > 0) ||
			(len(netInterface.IPV6.IPAddress) > 0 && len(netInterface.IPV6.IPAddress[0].PublicIP) > 0) {
			// Return if public IP address has already part of instance metadata.
			return instanceMetadata, nil
		}

		loadBalancerMetadata, err := ims.getLoadBalancerMetadata()
		if err != nil || loadBalancerMetadata == nil || loadBalancerMetadata.LoadBalancer == nil {
			// Log a warning since loadbalancer metadata may not be available when the VM
			// is not in standard LoadBalancer backend address pool.
			logger.V(4).Info("Warning: failed to get loadbalancer metadata", "error", err)
			return instanceMetadata, nil
		}

		publicIPs := loadBalancerMetadata.LoadBalancer.PublicIPAddresses
		fillNetInterfacePublicIPs(publicIPs, netInterface)
	}

	return instanceMetadata, nil
}

func (ims *InstanceMetadataService) getInstanceMetadata(_ string) (*InstanceMetadata, error) {
	req, err := http.NewRequest("GET", ims.imdsServer+consts.ImdsInstanceURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Metadata", "True")
	req.Header.Add("User-Agent", "golang/kubernetes-cloud-provider")

	q := req.URL.Query()
	q.Add("format", "json")
	q.Add("api-version", consts.ImdsInstanceAPIVersion)
	req.URL.RawQuery = q.Encode()

	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failure of getting instance metadata with response %q", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	obj := InstanceMetadata{}
	err = json.Unmarshal(data, &obj)
	if err != nil {
		return nil, err
	}

	return &obj, nil
}

func (ims *InstanceMetadataService) getLoadBalancerMetadata() (*LoadBalancerMetadata, error) {
	req, err := http.NewRequest("GET", ims.imdsServer+consts.ImdsLoadBalancerURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Metadata", "True")
	req.Header.Add("User-Agent", "golang/kubernetes-cloud-provider")

	q := req.URL.Query()
	q.Add("format", "json")
	q.Add("api-version", consts.ImdsLoadBalancerAPIVersion)
	req.URL.RawQuery = q.Encode()

	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failure of getting loadbalancer metadata with response %q", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	obj := LoadBalancerMetadata{}
	err = json.Unmarshal(data, &obj)
	if err != nil {
		return nil, err
	}

	return &obj, nil
}

// GetMetadata gets instance metadata from cache.
// crt determines if we can get data from stalled cache/need fresh if cache expired.
func (ims *InstanceMetadataService) GetMetadata(ctx context.Context, crt azcache.AzureCacheReadType) (*InstanceMetadata, error) {
	cache, err := ims.imsCache.Get(ctx, consts.MetadataCacheKey, crt)
	if err != nil {
		return nil, err
	}

	// Cache shouldn't be nil, but added a check in case something is wrong.
	if cache == nil {
		return nil, fmt.Errorf("failure of getting instance metadata")
	}

	if metadata, ok := cache.(*InstanceMetadata); ok {
		return metadata, nil
	}

	return nil, fmt.Errorf("failure of getting instance metadata")
}

// GetPlatformSubFaultDomain returns the PlatformSubFaultDomain from IMDS if set.
func (az *Cloud) GetPlatformSubFaultDomain(ctx context.Context) (string, error) {
	logger := log.FromContextOrBackground(ctx).WithName("GetPlatformSubFaultDomain")
	if az.UseInstanceMetadata {
		metadata, err := az.Metadata.GetMetadata(ctx, azcache.CacheReadTypeUnsafe)
		if err != nil {
			logger.Error(err, "failed to GetMetadata")
			return "", err
		}
		if metadata.Compute == nil {
			_ = az.Metadata.imsCache.Delete(consts.MetadataCacheKey)
			return "", errors.New("failure of getting compute information from instance metadata")
		}
		return metadata.Compute.PlatformSubFaultDomain, nil
	}
	return "", nil
}

type metadataLabelRule struct {
	name       string
	label      string
	expression string
}

type compiledMetadataLabelRule struct {
	name    string
	label   string
	program cel.Program
}

// interconnectLabelExpression builds the CEL expression for one interconnect
// label. When either M2 ID is present, it selects this label's M2 compute field
// (m2Field), keeping M2 self-consistent without mixing in legacy tags.
// Otherwise it falls back to the legacy M1 group tag, which represents a
// subgroup and is aliased onto both the group and subgroup labels.
func interconnectLabelExpression(m2Field string) string {
	return fmt.Sprintf(
		"compute.interconnectGroupId != '' || compute.interconnectSubgroupId != '' ? compute[%q] : (%q in tags ? tags[%q] : '')",
		m2Field, consts.TagNameInterconnectGroup, consts.TagNameInterconnectGroup)
}

// Initialize on the first IMDS label request, retaining compilation errors for callers.
// Programs are immutable and shared across requests; expressions are not user-configurable.
var builtinMetadataLabelRules = sync.OnceValues(func() ([]compiledMetadataLabelRule, error) {
	rules := []metadataLabelRule{
		{
			name:       "interconnect-group",
			label:      consts.LabelPlatformInterconnectGroup,
			expression: interconnectLabelExpression("interconnectGroupId"),
		},
		{
			name:       "interconnect-subgroup",
			label:      consts.LabelPlatformInterconnectSubgroup,
			expression: interconnectLabelExpression("interconnectSubgroupId"),
		},
	}
	// Keep built-in rules aligned with the registry the node manager trusts, so
	// a new rule cannot silently produce a label the consumer will reject.
	for _, rule := range rules {
		if _, ok := consts.ManagedMetadataLabelKeys[rule.label]; !ok {
			return nil, fmt.Errorf("built-in metadata label rule %q produces unregistered label %q", rule.name, rule.label)
		}
	}
	return compileMetadataLabelRules(rules)
})

func compileMetadataLabelRules(rules []metadataLabelRule) ([]compiledMetadataLabelRule, error) {
	env, err := cel.NewEnv(
		cel.Variable("tags", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("compute", cel.MapType(cel.StringType, cel.StringType)),
	)
	if err != nil {
		return nil, fmt.Errorf("create metadata label CEL environment: %w", err)
	}
	compiled := make([]compiledMetadataLabelRule, 0, len(rules))
	for _, rule := range rules {
		ast, issues := env.Compile(rule.expression)
		if issues.Err() != nil {
			return nil, fmt.Errorf("compile metadata label rule %q (%s): %w", rule.name, rule.label, issues.Err())
		}
		if !ast.OutputType().IsExactType(cel.StringType) {
			return nil, fmt.Errorf("metadata label rule %q (%s) must return string, got %s", rule.name, rule.label, ast.OutputType())
		}
		program, err := env.Program(ast, cel.CostLimit(consts.MetadataLabelEvaluationCostLimit))
		if err != nil {
			return nil, fmt.Errorf("create metadata label rule %q (%s): %w", rule.name, rule.label, err)
		}
		compiled = append(compiled, compiledMetadataLabelRule{name: rule.name, label: rule.label, program: program})
	}
	return compiled, nil
}

func evaluateMetadataLabels(ctx context.Context, rules []compiledMetadataLabelRule, compute ComputeMetadata) (map[string]string, error) {
	tags := make(map[string]string, len(compute.TagsList))
	for _, tag := range compute.TagsList {
		// IMDS historically used the first matching tag, including an empty value.
		if _, exists := tags[tag.Name]; !exists {
			tags[tag.Name] = tag.Value
		}
	}
	// Normalize supported optional fields so missing IDs are explicit empty strings in CEL.
	input := map[string]any{
		"tags": tags,
		"compute": map[string]string{
			"interconnectGroupId":    compute.InterconnectGroupID,
			"interconnectSubgroupId": compute.InterconnectSubgroupID,
		},
	}
	labels := make(map[string]string, len(rules))
	for _, rule := range rules {
		value, _, err := rule.program.ContextEval(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("evaluate metadata label rule %q (%s): %w", rule.name, rule.label, err)
		}
		text, ok := value.(types.String)
		if !ok {
			return nil, fmt.Errorf("metadata label rule %q (%s) returned a non-string value", rule.name, rule.label)
		}
		if text == "" {
			continue
		}
		if problems := validation.IsValidLabelValue(string(text)); len(problems) > 0 {
			return nil, fmt.Errorf("metadata label rule %q (%s) produced an invalid label value: %s", rule.name, rule.label, strings.Join(problems, "; "))
		}
		labels[rule.label] = string(text)
	}
	return labels, nil
}

// GetMetadataLabels evaluates built-in label rules against one cached IMDS snapshot.
// Empty rule results are omitted. No metadata labels are returned when IMDS is disabled.
func (az *Cloud) GetMetadataLabels(ctx context.Context) (map[string]string, error) {
	if !az.UseInstanceMetadata {
		return nil, nil
	}
	metadata, err := az.Metadata.GetMetadata(ctx, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return nil, err
	}
	if metadata.Compute == nil {
		_ = az.Metadata.imsCache.Delete(consts.MetadataCacheKey)
		return nil, errors.New("failure of getting compute information from instance metadata")
	}
	rules, err := builtinMetadataLabelRules()
	if err != nil {
		return nil, err
	}
	return evaluateMetadataLabels(ctx, rules, *metadata.Compute)
}

// GetInterconnectGroupID returns the M2 group ID or legacy M1 group tag from IMDS if set.
// Retained for callers of the original single-label API.
func (az *Cloud) GetInterconnectGroupID(ctx context.Context) (string, error) {
	labels, err := az.GetMetadataLabels(ctx)
	if err != nil {
		return "", err
	}
	return labels[consts.LabelPlatformInterconnectGroup], nil
}
