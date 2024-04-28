/*
Copyright 2022 The Kubernetes Authors.

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

package deployer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armcontainerservicev2 "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/kubetest2/pkg/exec"
)

var (
	apiVersion           = "2022-09-01"
	defaultKubeconfigDir = "_kubeconfig"
	usageTag             = "aks-cluster-e2e"
	cred                 *azidentity.DefaultAzureCredential
	onceWrapper          sync.Once
)

type UpOptions struct {
	ClusterName      string `flag:"clusterName" desc:"--clusterName flag for aks cluster name"`
	Location         string `flag:"location" desc:"--location flag for resource group and cluster location"`
	ConfigPath       string `flag:"config" desc:"--config flag for AKS cluster"`
	CustomConfigPath string `flag:"customConfig" desc:"--customConfig flag for custom configuration"`
	K8sVersion       string `flag:"k8sVersion" desc:"--k8sVersion flag for cluster Kubernetes version"`

	CCMImageTag        string `flag:"ccmImageTag" desc:"--ccmImageTag flag for CCM image tag"`
	CNMImageTag        string `flag:"cnmImageTag" desc:"--cnmImageTag flag for CNM image tag"`
	CASImageTag        string `flag:"casImageTag" desc:"--casImageTag flag for CAS image tag"`
	AzureDiskImageTag  string `flag:"azureDiskImageTag" desc:"--azureDiskImageTag flag for Azure disk image tag"`
	AzureFileImageTag  string `flag:"azureFileImageTag" desc:"--azureFileImageTag flag for Azure file image tag"`
	KubernetesImageTag string `flag:"kubernetesImageTag" desc:"--kubernetesImageTag flag for Kubernetes image tag"`
	KubeletURL         string `flag:"kubeletURL" desc:"--kubeletURL flag for kubelet binary URL"`
}

type enableCustomFeaturesPolicy struct{}

type changeLocationPolicy struct{}

type addCustomConfigPolicy struct {
	customConfig string
}

func (p enableCustomFeaturesPolicy) Do(req *policy.Request) (*http.Response, error) {
	req.Raw().Header.Add("AKSHTTPCustomFeatures", "Microsoft.ContainerService/EnableCloudControllerManager")
	return req.Next()
}

func (p changeLocationPolicy) Do(req *policy.Request) (*http.Response, error) {
	if strings.Contains(req.Raw().URL.String(), "Microsoft.ContainerService/locations") {
		q := req.Raw().URL.Query()
		q.Set("api-version", "2017-08-31")
		req.Raw().URL.RawQuery = q.Encode()
	}

	return req.Next()
}

func (p addCustomConfigPolicy) Do(req *policy.Request) (*http.Response, error) {
	if req.Raw().Method != http.MethodPut {
		return req.Next()
	}

	body, err := io.ReadAll(req.Raw().Body)
	if err != nil {
		return nil, err
	}
	if err := req.Raw().Body.Close(); err != nil {
		return nil, fmt.Errorf("failed to close req.Raw().Body: %w", err)
	}

	var managedCluster map[string]interface{}
	if err = json.Unmarshal([]byte(body), &managedCluster); err != nil {
		return nil, fmt.Errorf("failed to unmarshal managed cluster config: %v", err)
	}

	propertiesMap, ok := managedCluster["properties"]
	if !ok {
		propertiesMap = make(map[string]interface{})
		managedCluster["properties"] = propertiesMap
	}
	pMap, ok := propertiesMap.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("")
	}
	pMap["encodedCustomConfiguration"] = []byte(p.customConfig)
	managedCluster["properties"] = pMap

	var marshalledManagedCluster []byte
	if marshalledManagedCluster, err = json.Marshal(managedCluster); err != nil {
		return nil, fmt.Errorf("failed to marshal managed cluster config: %v", err)
	}

	readSeekerCloser := streaming.NopCloser(bytes.NewReader(marshalledManagedCluster))
	req.SetBody(readSeekerCloser, "application/json")

	return req.Next()
}

func runCmd(cmd exec.Cmd) error {
	exec.InheritOutput(cmd)
	return cmd.Run()
}

func init() {
	onceWrapper.Do(func() {
		var err error
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			klog.Fatalf("failed to authenticate: %v", err)
		}
	})
}

// Define the function to create a resource group.
func (d *deployer) createResourceGroup(subscriptionID string) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error) {
	rgClient, _ := armresources.NewResourceGroupsClient(subscriptionID, cred, nil)

	now := time.Now()
	timestamp := now.Unix()
	param := armresources.ResourceGroup{
		Location: pointer.String(d.Location),
		Tags: map[string]*string{
			"creation_date": pointer.String(fmt.Sprintf("%d", timestamp)),
			"usage":         pointer.String(usageTag),
		},
	}

	return rgClient.CreateOrUpdate(ctx, d.ResourceGroupName, param, nil)
}

func openPath(path string) ([]byte, error) {
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		return os.ReadFile(path)
	}
	resp, err := http.Get(path)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to http get url: %s", path)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return []byte{}, fmt.Errorf("failed to http get url with StatusCode: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (d *deployer) prepareCustomConfig() ([]byte, error) {
	customConfig, err := openPath(d.CustomConfigPath)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to read custom config file at %q: %v", d.CustomConfigPath, err)
	}

	imageMap := map[string]string{}

	prefix := imageRegistry
	if registryURL != "" && registryRepo != "" {
		prefix = fmt.Sprintf("%s/%s", registryURL, registryRepo)
	}
	imageMap["{CUSTOM_CCM_IMAGE}"] = fmt.Sprintf("%s/azure-cloud-controller-manager:%s", prefix, d.CCMImageTag)
	imageMap["{CUSTOM_CNM_IMAGE}"] = fmt.Sprintf("%s/azure-cloud-node-manager:%s-linux-amd64", prefix, d.CNMImageTag)
	imageMap["{CUSTOM_CAS_IMAGE}"] = fmt.Sprintf("%s/autoscaler/cluster-autoscaler:%s", prefix, d.CASImageTag)
	imageMap["{CUSTOM_AZURE_DISK_IMAGE}"] = fmt.Sprintf("%s/azuredisk-csi:%s", prefix, d.AzureDiskImageTag)
	imageMap["{CUSTOM_AZURE_FILE_IMAGE}"] = fmt.Sprintf("%s/azurefile-csi:%s", prefix, d.AzureFileImageTag)
	imageMap["{CUSTOM_KUBE_APISERVER_IMAGE}"] = fmt.Sprintf("%s/kube-apiserver:%s", prefix, d.KubernetesImageTag)
	imageMap["{CUSTOM_KCM_IMAGE}"] = fmt.Sprintf("%s/kube-controller-manager:%s", prefix, d.KubernetesImageTag)
	imageMap["{CUSTOM_KUBE_PROXY_IMAGE}"] = fmt.Sprintf("%s/kube-proxy:%s", prefix, d.KubernetesImageTag)
	imageMap["{CUSTOM_KUBE_SCHEDULER_IMAGE}"] = fmt.Sprintf("%s/kube-scheduler:%s", prefix, d.KubernetesImageTag)
	imageMap["{CUSTOM_KUBELET_URL}"] = d.KubeletURL

	for k, v := range imageMap {
		customConfig = bytes.ReplaceAll(customConfig, []byte(k), []byte(v))
	}
	return customConfig, nil
}

// prepareClusterConfig generates cluster config.
func (d *deployer) prepareClusterConfig(clusterID string) (*armcontainerservicev2.ManagedCluster, string, error) {
	configFile, err := openPath(d.ConfigPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read cluster config file at %q: %v", d.ConfigPath, err)
	}
	clusterConfig := string(configFile)
	clusterConfigMap := map[string]string{
		"{AKS_CLUSTER_ID}":     clusterID,
		"{CLUSTER_NAME}":       d.ClusterName,
		"{AZURE_LOCATION}":     d.Location,
		"{KUBERNETES_VERSION}": d.K8sVersion,
	}
	for k, v := range clusterConfigMap {
		clusterConfig = strings.ReplaceAll(clusterConfig, k, v)
	}

	customConfig, err := d.prepareCustomConfig()
	if err != nil {
		return nil, "", fmt.Errorf("failed to prepare custom config: %v", err)
	}

	klog.Infof("Customized configurations are: %s", string(customConfig))

	encodedCustomConfig := base64.StdEncoding.EncodeToString(customConfig)
	clusterConfig = strings.ReplaceAll(clusterConfig, "{CUSTOM_CONFIG}", encodedCustomConfig)

	klog.Infof("AKS cluster config without credential: %s", clusterConfig)

	mcConfig := &armcontainerservicev2.ManagedCluster{}
	err = json.Unmarshal([]byte(clusterConfig), mcConfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal cluster config: %v", err)
	}
	updateAzureCredential(mcConfig)

	return mcConfig, string(customConfig), nil
}

func updateAzureCredential(mcConfig *armcontainerservicev2.ManagedCluster) {
	klog.Infof("Updating Azure credentials to manage cluster resource group")

	if len(clientID) != 0 && len(clientSecret) != 0 {
		klog.Infof("Service principal is used to manage cluster resource group")
		// Reset `Identity` in case managed identity is defined in templates while service principal is used.
		mcConfig.Identity = nil
		mcConfig.Properties.ServicePrincipalProfile = &armcontainerservicev2.ManagedClusterServicePrincipalProfile{
			ClientID: &clientID,
			Secret:   &clientSecret,
		}
		return
	}
	// Managed identity is preferable over service principal and picked by default when creating an AKS cluster.
	klog.Infof("Managed identity is used to manage cluster resource group")
	// Reset `ServicePrincipalProfile` in case service principal is defined in templates while managed identity is used.
	mcConfig.Properties.ServicePrincipalProfile = nil
	userAssignedIdentity := armcontainerservicev2.ResourceIdentityTypeUserAssigned
	mcConfig.Identity = &armcontainerservicev2.ManagedClusterIdentity{
		Type: &userAssignedIdentity,
	}
}

// createAKSWithCustomConfig creates an AKS cluster with custom configuration.
func (d *deployer) createAKSWithCustomConfig() error {
	klog.Infof("Creating the AKS cluster with custom config")
	clusterID := fmt.Sprintf("/subscriptions/%s/resourcegroups/%s/providers/Microsoft.ContainerService/managedClusters/%s", subscriptionID, d.ResourceGroupName, d.ClusterName)

	mcConfig, encodedCustomConfig, err := d.prepareClusterConfig(clusterID)
	if err != nil {
		return fmt.Errorf("failed to prepare cluster config: %v", err)
	}
	options := arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			APIVersion: apiVersion,
			PerCallPolicies: []policy.Policy{
				&enableCustomFeaturesPolicy{},
				&changeLocationPolicy{},
				&addCustomConfigPolicy{customConfig: encodedCustomConfig},
			},
		},
		DisableRPRegistration: false,
	}
	client, err := armcontainerservicev2.NewManagedClustersClient(subscriptionID, cred, &options)
	if err != nil {
		return fmt.Errorf("failed to new managed cluster client with sub ID %q: %v", subscriptionID, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	poller, err := client.BeginCreateOrUpdate(ctx, d.ResourceGroupName, d.ClusterName, *mcConfig, nil)
	if err != nil {
		return fmt.Errorf("failed to put resource: %v", err.Error())
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to put resource: %v", err.Error())
	}

	klog.Infof("An AKS cluster %q in resource group %q is created", d.ClusterName, d.ResourceGroupName)
	return nil
}

// getAKSKubeconfig gets kubeconfig of the AKS cluster and writes it to specific path.
func (d *deployer) getAKSKubeconfig() error {
	klog.Infof("Retrieving AKS cluster's kubeconfig")
	client, err := armcontainerservicev2.NewManagedClustersClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to new managed cluster client with sub ID %q: %v", subscriptionID, err)
	}

	var resp armcontainerservicev2.ManagedClustersClientListClusterUserCredentialsResponse
	err = wait.PollImmediate(1*time.Minute, 30*time.Minute, func() (done bool, err error) {
		resp, err = client.ListClusterUserCredentials(ctx, d.ResourceGroupName, d.ClusterName, nil)
		if err != nil {
			if strings.Contains(err.Error(), "404 Not Found") {
				klog.Infof("failed to list cluster user credentials for 1 minute, retrying")
				return false, nil
			}
			return false, fmt.Errorf("failed to list cluster user credentials with resource group name %q, cluster ID %q: %v", d.ResourceGroupName, d.ClusterName, err)
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	kubeconfigs := resp.CredentialResults.Kubeconfigs
	if len(kubeconfigs) == 0 {
		return fmt.Errorf("failed to find a valid kubeconfig")
	}
	kubeconfig := kubeconfigs[0]
	destPath := fmt.Sprintf("%s/%s_%s.kubeconfig", defaultKubeconfigDir, d.ResourceGroupName, d.ClusterName)

	if err := os.MkdirAll(defaultKubeconfigDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to mkdir the default kubeconfig dir: %v", err)
	}
	if err := os.WriteFile(destPath, kubeconfig.Value, 0666); err != nil {
		return fmt.Errorf("failed to write kubeconfig to %s", destPath)
	}

	klog.Infof("Succeeded in getting kubeconfig of cluster %q in resource group %q", d.ClusterName, d.ResourceGroupName)
	return nil
}

func (d *deployer) verifyUpFlags() error {
	if d.ResourceGroupName == "" {
		return fmt.Errorf("resource group name is empty")
	}
	if d.Location == "" {
		return fmt.Errorf("location is empty")
	}
	if d.ClusterName == "" {
		d.ClusterName = "aks-cluster"
	}
	if d.ConfigPath == "" {
		return fmt.Errorf("cluster config path is empty")
	}
	if d.CustomConfigPath == "" {
		return fmt.Errorf("custom config path is empty")
	}
	if d.K8sVersion == "" {
		return fmt.Errorf("k8s version is empty")
	}

	if d.CNMImageTag == "" {
		d.CNMImageTag = d.CCMImageTag
	}
	return nil
}

func (d *deployer) Up() error {
	if err := d.verifyUpFlags(); err != nil {
		return fmt.Errorf("up flags are invalid: %v", err)
	}

	// Create the resource group
	resourceGroup, err := d.createResourceGroup(subscriptionID)
	if err != nil {
		return fmt.Errorf("failed to create the resource group: %v", err)
	}
	klog.Infof("Resource group %s created", *resourceGroup.ResourceGroup.ID)

	// Create the AKS cluster
	if err := d.createAKSWithCustomConfig(); err != nil {
		return fmt.Errorf("failed to create the AKS cluster: %v", err)
	}

	// Get the cluster kubeconfig
	if err := d.getAKSKubeconfig(); err != nil {
		return fmt.Errorf("failed to get AKS cluster kubeconfig: %v", err)
	}
	return nil
}

func (d *deployer) IsUp() (up bool, err error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		klog.Fatalf("failed to authenticate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := armcontainerservicev2.NewManagedClustersClient(subscriptionID, cred, nil)
	if err != nil {
		return false, fmt.Errorf("failed to new managed cluster client with sub ID %q: %v", subscriptionID, err)
	}
	managedCluster, rerr := client.Get(ctx, d.ResourceGroupName, d.ClusterName, nil)
	if rerr != nil {
		return false, fmt.Errorf("failed to get managed cluster %q in resource group %q: %v", d.ClusterName, d.ResourceGroupName, rerr.Error())
	}

	return managedCluster.Properties.ProvisioningState != nil && *managedCluster.Properties.ProvisioningState == "Succeeded", nil
}
