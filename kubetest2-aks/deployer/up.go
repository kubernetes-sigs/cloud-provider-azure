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
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armcontainerservicev2 "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/containerserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"

	"sigs.k8s.io/kubetest2/pkg/exec"
)

var (
	apiVersion           = "2022-04-02-preview"
	defaultKubeconfigDir = "_kubeconfig"
	usageTag             = "aks-cluster-e2e"
)

type UpOptions struct {
	ClusterName      string `flag:"clusterName" desc:"--clusterName flag for aks cluster name"`
	Location         string `flag:"location" desc:"--location flag for resource group and cluster location"`
	CCMImageTag      string `flag:"ccmImageTag" desc:"--ccmImageTag flag for CCM image tag"`
	ConfigPath       string `flag:"config" desc:"--config flag for AKS cluster"`
	CustomConfigPath string `flag:"customConfig" desc:"--customConfig flag for custom configuration"`
	K8sVersion       string `flag:"k8sVersion" desc:"--k8sVersion flag for cluster Kubernetes version"`
}

func runCmd(cmd exec.Cmd) error {
	exec.InheritOutput(cmd)
	return cmd.Run()
}

// Define the function to create a resource group.
func (d *deployer) createResourceGroup(subscriptionID string, credential azcore.TokenCredential) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error) {
	rgClient, _ := armresources.NewResourceGroupsClient(subscriptionID, credential, nil)

	now := time.Now()
	timestamp := now.Unix()
	param := armresources.ResourceGroup{
		Location: to.StringPtr(d.Location),
		Tags: map[string]*string{
			"creation_date": to.StringPtr(fmt.Sprintf("%d", timestamp)),
			"usage":         to.StringPtr(usageTag),
		},
	}

	return rgClient.CreateOrUpdate(ctx, d.ResourceGroupName, param, nil)
}

// prepareClusterConfig generates cluster config.
func (d *deployer) prepareClusterConfig(imageTag string, clusterID string) (string, error) {
	configFile, err := ioutil.ReadFile(d.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to read cluster config file at %q: %v", d.ConfigPath, err)
	}
	clusterConfig := string(configFile)
	clusterConfigMap := map[string]string{
		"{AKS_CLUSTER_ID}":      clusterID,
		"{CLUSTER_NAME}":        d.ClusterName,
		"{AZURE_LOCATION}":      d.Location,
		"{AZURE_CLIENT_ID}":     clientID,
		"{AZURE_CLIENT_SECRET}": clientSecret,
		"{KUBERNETES_VERSION}":  d.K8sVersion,
	}
	for k, v := range clusterConfigMap {
		clusterConfig = strings.ReplaceAll(clusterConfig, k, v)
	}

	customConfig, err := ioutil.ReadFile(d.CustomConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to read custom config file at %q: %v", d.CustomConfigPath, err)
	}

	cloudProviderImageMap := map[string]string{
		"{CUSTOM_CCM_IMAGE}": fmt.Sprintf("%s/azure-cloud-controller-manager:%s", imageRegistry, imageTag),
		"{CUSTOM_CNM_IMAGE}": fmt.Sprintf("%s/azure-cloud-node-manager:%s-linux-amd64", imageRegistry, imageTag),
	}
	for k, v := range cloudProviderImageMap {
		customConfig = bytes.ReplaceAll(customConfig, []byte(k), []byte(v))
	}
	klog.Infof("AKS cluster custom config: %s", customConfig)

	encodedCustomConfig := base64.StdEncoding.EncodeToString(customConfig)
	clusterConfig = strings.ReplaceAll(clusterConfig, "{CUSTOM_CONFIG}", encodedCustomConfig)

	return clusterConfig, nil
}

func (d *deployer) getAzureClientConfig() (*azclients.ClientConfig, error) {
	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(azure.PublicCloud.ActiveDirectoryEndpoint, tenantID, &apiVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to new oath config with api version: %v", err)
	}
	spToken, err := adal.NewServicePrincipalToken(*oauthConfig, clientID, clientSecret, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to new service principal token: %v", err)
	}

	authorizer := autorest.NewBearerAuthorizer(spToken)
	baseURL := azure.PublicCloud.ResourceManagerEndpoint
	azClientConfig := azclients.ClientConfig{
		CloudName:               azure.PublicCloud.Name,
		Location:                d.Location,
		SubscriptionID:          subscriptionID,
		ResourceManagerEndpoint: baseURL,
		Authorizer:              authorizer,
		Backoff:                 &retry.Backoff{Steps: 1},
	}
	return &azClientConfig, nil
}

func (d *deployer) newArmClient() (*armclient.Client, error) {
	config, err := d.getAzureClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Azure client config: %v", err)
	}

	return armclient.New(config.Authorizer, *config, config.ResourceManagerEndpoint, apiVersion), nil
}

// createAKSWithCustomConfig creates an AKS cluster with custom configuration.
func (d *deployer) createAKSWithCustomConfig(token string, imageTag string) error {
	klog.Infof("Creating the AKS cluster with custom config")
	clusterID := fmt.Sprintf("/subscriptions/%s/resourcegroups/%s/providers/Microsoft.ContainerService/managedClusters/%s", subscriptionID, d.ResourceGroupName, d.ClusterName)

	clusterConfig, err := d.prepareClusterConfig(imageTag, clusterID)
	if err != nil {
		return fmt.Errorf("failed to prepare cluster config: %v", err)
	}
	klog.Infof("AKS cluster config: %s", clusterConfig)

	decorators := []autorest.PrepareDecorator{
		autorest.WithHeader("Authorization", fmt.Sprintf("Bearer %s", token)),
		autorest.WithHeader("Content-Type", "application/json"),
		autorest.WithHeader("AKSHTTPCustomFeatures", "Microsoft.ContainerService/EnableCloudControllerManager"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var unmarshalledClusterConfig interface{}
	if err := json.Unmarshal([]byte(clusterConfig), &unmarshalledClusterConfig); err != nil {
		return fmt.Errorf("failed to unmarshal cluster config: %v", err)
	}

	armClient, err := d.newArmClient()
	if err != nil {
		return fmt.Errorf("failed to new arm client: %v", err)
	}

	resp, rerr := armClient.PutResource(ctx, clusterID, unmarshalledClusterConfig, decorators...)
	defer armClient.CloseResponse(ctx, resp)
	if rerr != nil {
		return fmt.Errorf("failed to put resource: %v", rerr.Error())
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to create the AKS cluster: output %v\nerr %v", resp, err)
	}

	klog.Infof("An AKS cluster %q in resource group %q is creating", d.ClusterName, d.ResourceGroupName)
	return nil
}

// getAKSKubeconfig gets kubeconfig of the AKS cluster and writes it to specific path.
func (d *deployer) getAKSKubeconfig(cred *azidentity.DefaultAzureCredential) error {
	klog.Infof("Retrieving AKS cluster's kubeconfig")
	client, err := armcontainerservicev2.NewManagedClustersClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to new managed cluster client with sub ID %q: %v", subscriptionID, err)
	}

	var resp armcontainerservicev2.ManagedClustersClientListClusterUserCredentialsResponse
	err = wait.PollImmediate(1*time.Minute, 20*time.Minute, func() (done bool, err error) {
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
	if err := ioutil.WriteFile(destPath, kubeconfig.Value, 0666); err != nil {
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
	if d.CCMImageTag == "" {
		return fmt.Errorf("ccm image tag is empty")
	}
	if d.K8sVersion == "" {
		return fmt.Errorf("k8s version is empty")
	}
	return nil
}

func (d *deployer) Up() error {
	if err := d.verifyUpFlags(); err != nil {
		return fmt.Errorf("up flags are invalid: %v", err)
	}

	// Create a credential object.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		klog.Fatalf("Authentication failure: %+v", err)
	}

	// Create the resource group
	resourceGroup, err := d.createResourceGroup(subscriptionID, cred)
	if err != nil {
		return fmt.Errorf("failed to create the resource group: %v", err)
	}
	klog.Infof("Resource group %s created", *resourceGroup.ResourceGroup.ID)

	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
	if err != nil {
		return fmt.Errorf("failed to get token from credential: %v", err)
	}

	// Create the AKS cluster
	if err := d.createAKSWithCustomConfig(token.Token, d.CCMImageTag); err != nil {
		return fmt.Errorf("failed to create the AKS cluster: %v", err)
	}

	// Wait for the cluster to be up
	if err := d.waitForClusterUp(); err != nil {
		return fmt.Errorf("failed to wait for cluster to be up: %v", err)
	}

	// Get the cluster kubeconfig
	if err := d.getAKSKubeconfig(cred); err != nil {
		return fmt.Errorf("failed to get AKS cluster kubeconfig: %v", err)
	}
	return nil
}

func (d *deployer) waitForClusterUp() error {
	klog.Infof("Waiting for AKS cluster to be up")
	config, err := d.getAzureClientConfig()
	if err != nil {
		return fmt.Errorf("failed to get client config: %v", err)
	}

	client := containerserviceclient.New(config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = wait.PollImmediate(10*time.Second, 10*time.Minute, func() (done bool, err error) {
		managedCluster, rerr := client.Get(ctx, d.ResourceGroupName, d.ClusterName)
		if rerr != nil {
			return false, fmt.Errorf("failed to get managed cluster %q in resource group %q: %v", d.ClusterName, d.ResourceGroupName, rerr.Error())
		}
		return managedCluster.ProvisioningState != nil && *managedCluster.ProvisioningState == "Succeeded", nil
	})
	return err
}

func (d *deployer) IsUp() (up bool, err error) {
	config, err := d.getAzureClientConfig()
	if err != nil {
		return false, fmt.Errorf("failed to get client config: %v", err)
	}
	client := containerserviceclient.New(config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managedCluster, rerr := client.Get(ctx, d.ResourceGroupName, d.ClusterName)
	if rerr != nil {
		return false, fmt.Errorf("failed to get managed cluster %q in resource group %q: %v", d.ClusterName, d.ResourceGroupName, rerr.Error())
	}

	return managedCluster.ProvisioningState != nil && *managedCluster.ProvisioningState == "Succeeded", nil
}
