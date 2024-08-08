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
	"io"
	"os"
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

// PushImageToACR pull an image from MCR and push
// it to the given azure container registry
func (tc *AzureTestClient) PushImageToACR(registryName, image string) (tag string, err error) {
	acrClient := tc.createACRClient()
	rgName := tc.GetResourceGroup()

	tag = "1.25"
	future, err := acrClient.ImportImage(context.Background(), rgName, registryName, acr.ImportImageParameters{
		Source: &acr.ImportSource{
			RegistryURI: pointer.String("mcr.microsoft.com"),
			SourceImage: pointer.String("mirror/docker/library/" + image + ":" + tag),
		},
		TargetTags: &[]string{
			image + ":" + tag,
		},
		Mode: acr.NoForce,
	})
	if err != nil {
		return "", fmt.Errorf("failed to import image %s from mcr to acr %s with error: %w", image, registryName, err)
	}
	err = future.WaitForCompletionRef(context.Background(), acrClient.Client)
	if err != nil {
		return "", fmt.Errorf("failed to import image %s from mcr to acr %s with error: %w", image, registryName, err)
	}
	return tag, nil
}

// AZACRLogin does az login and then az acr login.
func AZACRLogin(acrName string) (err error) {
	authConfig, err := azureAuthConfigFromTestProfile()
	if err != nil {
		return err
	}

	Logf("Attempting az login with azure cred.")
	authFlags := []string{
		"--username", authConfig.AADClientID,
		"--password", authConfig.AADClientSecret,
	}
	if authConfig.UseFederatedWorkloadIdentityExtension {
		tokenFile, err := os.Open(authConfig.AADFederatedTokenFile)
		if err != nil {
			return err
		}
		token, err := io.ReadAll(tokenFile)
		if err != nil {
			return err
		}
		authFlags = []string{
			"--username", authConfig.AADClientID,
			"--federated-token", string(token),
		}
	}
	args := []string{
		"login", "--service-principal",
		"--tenant", authConfig.TenantID,
	}
	//nolint:gosec // G204 ignore this!
	cmd := exec.Command("az", append(args, authFlags...)...)
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("az failed to login with error: %w", err)
	}
	Logf("az login success.")

	cmd = exec.Command("az", "account", "show")
	var output []byte
	if output, err = cmd.Output(); err != nil {
		return fmt.Errorf("az failed to account show with output: %s\n error: %w", string(output), err)
	}
	Logf("az account show success.")

	Logf("Attempting az acr login with azure cred.")
	cmd = exec.Command("az", "acr", "login",
		"-n", acrName)
	if output, err = cmd.Output(); err != nil {
		return fmt.Errorf("az acr failed to login with output %s\n error: %w", string(output), err)
	}
	Logf("az acr login success %q.", string(output))

	return nil
}

// AZLogout does az logout.
func AZLogout() (err error) {
	Logf("Attempting az logout.")
	cmd := exec.Command("az", "logout")
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("az failed to logout with error: %w", err)
	}
	Logf("az logout success.")
	return nil
}

// AZACRCacheCreate enables acr cache for a image.
func AZACRCacheCreate(acrName, ruleName, imageURL, imageName string) (err error) {
	Logf("Attempting az acr cache create for image URL %q.", imageURL)
	cmd := exec.Command("az", "acr", "cache", "create",
		"-r", acrName,
		"-n", ruleName,
		"-s", imageURL,
		"-t", imageName)
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("az acr cache create failed with error: %w", err)
	}
	Logf("az acr cache create success.")
	return nil
}
