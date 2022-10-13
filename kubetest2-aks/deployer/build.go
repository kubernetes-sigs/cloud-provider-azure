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
	"fmt"
	"os"

	git "github.com/go-git/go-git/v5"
	plumbing "github.com/go-git/go-git/v5/plumbing"
	"k8s.io/klog"

	"sigs.k8s.io/kubetest2/pkg/exec"
)

var (
	customConfigComponents = map[string]string{
		"ccm":        "https://github.com/kubernetes-sigs/cloud-provider-azure.git",
		"cnm":        "https://github.com/kubernetes-sigs/cloud-provider-azure.git",
		"azure-file": "https://github.com/kubernetes-sigs/azurefile-csi-driver.git",
		"azure-disk": "https://github.com/kubernetes-sigs/azuredisk-csi-driver.git",
	}
	gitClonePath string = "_git"
)

type BuildOptions struct {
	// Target must be set. Only one of TargetPath and TargetTag should be set.
	Target     string `flag:"target" desc:"--target flag for custom config component to test, e.g. cloud-provider-azure"`
	TargetPath string `flag:"targetPath" desc:"--targetPath flag for local repo, not set with TargetCommit or TargetFlag"`
	TargetTag  string `flag:"targetTag" desc:"--targetTag flag for custom config component's refs"`
}

func (d *deployer) verifyBuildFlags() error {
	if _, ok := customConfigComponents[d.Target]; !ok {
		return fmt.Errorf("component %q not supported", d.Target)
	}

	if (d.TargetPath != "" && d.TargetTag != "") || (d.TargetPath == "" && d.TargetTag == "") {
		return fmt.Errorf("only one of TargetPath and TargetTag should be set")
	}

	return nil
}

func (d *deployer) makeCloudProviderImages(path string) (string, error) {
	// Show commit
	if err := runCmd(exec.Command("git", "-C", path, "show", "--stat")); err != nil {
		return "", fmt.Errorf("failed to show commit: %v", err)
	}

	// Make images
	targets := []string{"build-ccm-image-amd64", "push-ccm-image-amd64"}
	if d.Target == "cnm" {
		targets = []string{"build-node-image-linux-amd64", "push-node-image-linux-amd64"}
	}
	for _, target := range targets {
		if err := runCmd(exec.Command("make", "-C", path, target)); err != nil {
			return "", fmt.Errorf("failed to make %s: %v", target, err)
		}
	}

	imageTag, err := exec.Output(exec.Command("git", "-C", path, "rev-parse", "--short=7", "HEAD"))
	if err != nil {
		return "", fmt.Errorf("failed to get image tag: %v", err)
	}

	return string(imageTag), nil
}

// makeCloudProviderImagesByPath makes CCM or CNM images with repo path.
func (d *deployer) makeCloudProviderImagesByPath() (string, error) {
	klog.Infof("Making Cloud provider images with repo path")

	path := d.TargetPath
	return d.makeCloudProviderImages(path)
}

// makeCloudProviderImagesByTag makes CCM or CNM images with repo refs.
func (d *deployer) makeCloudProviderImagesByTag(url string) (string, error) {
	klog.Infof("Making Cloud provider images with refs")
	ccmPath := fmt.Sprintf("%s/cloud-provider-azure", gitClonePath)

	repo, err := git.PlainClone(ccmPath, false, &git.CloneOptions{
		URL:      url,
		Progress: os.Stdout,
	})
	if err != nil {
		return "", fmt.Errorf("failed to clone from URL %q", url)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %v", err)
	}
	worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", d.TargetTag)),
	})

	return d.makeCloudProviderImages(ccmPath)
}

func (d *deployer) Build() error {
	err := d.verifyBuildFlags()
	if err != nil {
		return fmt.Errorf("failed to verify build flags: %v", err)
	}

	if d.Target == "ccm" || d.Target == "cnm" {
		var imageTag string
		if d.TargetPath != "" {
			if imageTag, err = d.makeCloudProviderImagesByPath(); err != nil {
				return fmt.Errorf("failed to make Cloud provider image with path %q: %v", d.TargetPath, err)
			}
		} else {
			if imageTag, err = d.makeCloudProviderImagesByTag(customConfigComponents[d.Target]); err != nil {
				return fmt.Errorf("failed to make Cloud provider image with tag %q: %v", d.TargetTag, err)
			}
		}
		klog.Infof("cloud-provider-azure image with tag %q are ready", imageTag)
	}

	return nil
}
