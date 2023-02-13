/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"
	"os/exec"

	acr "github.com/Azure/azure-sdk-for-go/services/containerregistry/mgmt/2019-05-01/containerregistry"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/pointer"
)

// CreateContainerRegistry creates a test acr
func (tc *AzureTestClient) CreateContainerRegistry() (registry acr.Registry, err error) {
	acrClient := tc.createACRClient()
	rgName, location := tc.GetResourceGroup(), tc.GetLocation()
	acrName := "e2eacr" + string(uuid.NewUUID())[0:4]
	template := acr.Registry{
		Sku: &acr.Sku{
			Name: "Basic",
			Tier: "SkuTierBasic",
		},
		Name:     pointer.String(acrName),
		Location: pointer.String(location),
	}

	Logf("Creating acr %s in resource group %s.", acrName, rgName)
	future, err := acrClient.Create(context.Background(), rgName, acrName, template)
	if err != nil {
		return acr.Registry{}, err
	}
	err = future.WaitForCompletionRef(context.Background(), acrClient.Client)
	if err != nil {
		return acr.Registry{}, err
	}
	return future.Result(*acrClient)
}

// DeleteContainerRegistry deletes an existing acr
func (tc *AzureTestClient) DeleteContainerRegistry(registryName string) (err error) {
	acrClient := tc.createACRClient()
	rgName := tc.GetResourceGroup()

	Logf("Deleting acr %s in resource group %s.", registryName, rgName)
	future, err := acrClient.Delete(context.Background(), rgName, registryName)
	if err != nil {
		return fmt.Errorf("failed to delete acr %s in resource group %s with error: %w", registryName, rgName, err)
	}
	err = future.WaitForCompletionRef(context.Background(), acrClient.Client)
	if err != nil {
		return fmt.Errorf("failed to delete acr %s in resource group %s with error: %w", registryName, rgName, err)
	}
	return nil
}

// DockerLogin execute the `docker login` if docker is available
func DockerLogin(registryName string) (err error) {
	authConfig, err := azureAuthConfigFromTestProfile()
	if err != nil {
		return err
	}

	cmd := exec.Command("docker", "-v")
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute docker command with error: %w", err)
	}

	Logf("Attempting Docker login with azure cred.")
	arg0 := "--username=" + authConfig.AADClientID
	arg1 := "--password=" + authConfig.AADClientSecret
	cmd = exec.Command("docker",
		"login",
		arg0,
		arg1,
		registryName+".azurecr.io")
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("docker failed to login with error: %w", err)
	}
	Logf("Docker login success.")
	return nil
}

// DockerLogout execute the `docker logout` if docker is available
func DockerLogout() (err error) {
	Logf("Docker logout.")
	cmd := exec.Command("docker", "logout")
	return cmd.Run()
}

// PushImageToACR pull an image from Docker Hub and push
// it to the given azure container registry
func PushImageToACR(registryName, image string) (tag string, err error) {
	Logf("Pulling %s from Docker Hub.", image)
	cmd := exec.Command("docker", "pull", image)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("failed pulling %s with error: %w", image, err)
	}

	Logf("Tagging image.")
	tagSuffix := string(uuid.NewUUID())[0:4]
	registry := fmt.Sprintf("%s.azurecr.io/%s:e2e-%s", registryName, image, tagSuffix)
	cmd = exec.Command("docker", "tag", image, registry)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("failed tagging nginx image with error: %w", err)
	}

	Logf("Pushing image to ACR.")
	cmd = exec.Command("docker", "push", registry)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("failed pushing %s image to registry %s with error: %w", image, registryName, err)
	}

	Logf("Pushing image success.")
	tag = fmt.Sprintf("e2e-%s", tagSuffix)
	return tag, nil
}

// AssignRoleToACR assigns the role to acr by roleDefinitionID
func (tc *AzureTestClient) AssignRoleToACR(registryName, roleDefinitionID string) (err error) {
	Logf("Assigning role to registry %s", registryName)
	authConfig := tc.GetAuthConfig()
	scopeFormat := "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerRegistry/registries/%s"
	scope := fmt.Sprintf(scopeFormat, authConfig.SubscriptionID, tc.GetResourceGroup(), registryName)

	_, err = tc.assignRoleByID(scope, roleDefinitionID)
	if err != nil {
		return err
	}
	return nil
}
