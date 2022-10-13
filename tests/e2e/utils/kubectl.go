/*
Copyright 2018 The Kubernetes Authors.

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
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	uexec "k8s.io/utils/exec"

	. "github.com/onsi/gomega"
)

// LookForStringInPodExec looks for the given string in the output of a command
// executed in the first container of specified pod.
func LookForStringInPodExec(ns, podName string, command []string, expectedString string, timeout time.Duration) (result string, err error) {
	return LookForStringInPodExecToContainer(ns, podName, "", command, expectedString, timeout)
}

// LookForStringInPodExecToContainer looks for the given string in the output of a
// command executed in specified pod container, or first container if not specified.
func LookForStringInPodExecToContainer(ns, podName, containerName string, command []string, expectedString string, timeout time.Duration) (result string, err error) {
	return lookForString(expectedString, timeout, func() string {
		args := []string{"exec", podName, fmt.Sprintf("--namespace=%v", ns)}
		if len(containerName) > 0 {
			args = append(args, fmt.Sprintf("--container=%s", containerName))
		}
		args = append(args, "--")
		args = append(args, command...)
		return RunKubectlOrDie(ns, args...)
	})
}

// lookForString looks for the given string in the output of fn, repeatedly calling fn until
// the timeout is reached or the string is found. Returns last log and possibly
// error if the string was not found.
func lookForString(expectedString string, timeout time.Duration, fn func() string) (result string, err error) {
	for t := time.Now(); time.Since(t) < timeout; time.Sleep(2 * time.Second) {
		result = fn()
		if strings.Contains(result, expectedString) {
			return
		}
	}
	err = fmt.Errorf("failed to find \"%s\", last result: \"%s\"", expectedString, result)
	return
}

// RunKubectlOrDie is a convenience wrapper over kubectlBuilder
func RunKubectlOrDie(namespace string, args ...string) string {
	return NewKubectlCommand(namespace, args...).ExecOrDie(namespace)
}

// RunKubectl is a convenience wrapper over kubectlBuilder
func RunKubectl(namespace string, args ...string) (string, error) {
	return NewKubectlCommand(namespace, args...).Exec()
}

// KubectlBuilder is used to build, customize and execute a kubectl Command.
// Add more functions to customize the builder as needed.
type KubectlBuilder struct {
	cmd     *exec.Cmd
	timeout <-chan time.Time
}

// NewKubectlCommand returns a KubectlBuilder for running kubectl.
func NewKubectlCommand(namespace string, args ...string) *KubectlBuilder {
	b := new(KubectlBuilder)
	b.cmd = KubectlCmd(namespace, args...)
	return b
}

// KubectlCmd runs the kubectl executable through the wrapper script.
func KubectlCmd(namespace string, args ...string) *exec.Cmd {
	defaultArgs := []string{}

	filename := findExistingKubeConfig()
	Logf("Kubernetes configuration file name: %s", filename)

	defaultArgs = append(defaultArgs, "--"+clientcmd.RecommendedConfigPathFlag+"="+filename)
	if namespace != "" {
		defaultArgs = append(defaultArgs, fmt.Sprintf("--namespace=%s", namespace))
	}
	kubectlArgs := append(defaultArgs, args...)

	cmd := exec.Command("kubectl", kubectlArgs...)

	//caller will invoke this and wait on it.
	return cmd
}

// ExecOrDie runs the kubectl executable or dies if error occurs.
func (b KubectlBuilder) ExecOrDie(namespace string) string {
	str, err := b.Exec()
	// In case of i/o timeout error, try talking to the apiserver again after 2s before dying.
	// Note that we're still dying after retrying so that we can get visibility to triage it further.
	if isTimeout(err) {
		Logf("Hit i/o timeout error, talking to the server 2s later to see if it's temporary.")
		time.Sleep(2 * time.Second)
		retryStr, retryErr := RunKubectl(namespace, "version")
		Logf("stdout: %q", retryStr)
		Logf("err: %v", retryErr)
	}
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	return str
}

// Exec runs the kubectl executable.
func (b KubectlBuilder) Exec() (string, error) {
	stdout, stderr, err := b.ExecWithFullOutput()
	return fmt.Sprintf("stdout:%s\nstderr:%s", stdout, stderr), err
}

// ExecWithFullOutput runs the kubectl executable, and returns the stdout and stderr.
func (b KubectlBuilder) ExecWithFullOutput() (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := b.cmd
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	Logf("Running '%s %s'", cmd.Path, strings.Join(cmd.Args[1:], " ")) // skip arg[0] as it is printed separately
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("error starting %v:\nCommand stdout:\n%v\nstderr:\n%v\nerror:\n%w", cmd, cmd.Stdout, cmd.Stderr, err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			var rc = 127
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.Sys().(syscall.WaitStatus).ExitStatus()
				Logf("rc: %d", rc)
			}
			return stdout.String(), stderr.String(), uexec.CodeExitError{
				Err:  fmt.Errorf("error running %v:\nCommand stdout:\n%v\nstderr:\n%v\nerror:\n%w", cmd, cmd.Stdout, cmd.Stderr, err),
				Code: rc,
			}
		}
	case <-b.timeout:
		_ = b.cmd.Process.Kill()
		return "", "", fmt.Errorf("timed out waiting for command %v:\nCommand stdout:\n%v\nstderr:\n%v", cmd, cmd.Stdout, cmd.Stderr)
	}
	Logf("stderr: %q", stderr.String())
	Logf("stdout: %q", stdout.String())
	return stdout.String(), stderr.String(), nil
}

func isTimeout(err error) bool {
	switch err := err.(type) {
	case *url.Error:
		if err, ok := err.Err.(net.Error); ok && err.Timeout() {
			return true
		}
	case net.Error:
		if err.Timeout() {
			return true
		}
	}
	return false
}

// ScaleMachinePool switches to kind-capz context and scales MachinePool replicas
// to scale up or down VMSS.
// This functions is for CAPZ clusters. Since CAPZ controller will reconcile MachinePool
// replicas automatically, add/delete VMSS instance through VMSS API doesn't work.
func ScaleMachinePool(vmssName string, instanceCount int64) error {
	kubeconfig := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)

	if err := os.Unsetenv(clientcmd.RecommendedConfigPathEnvVar); err != nil {
		return fmt.Errorf("failed to unset %s: %w", clientcmd.RecommendedConfigPathEnvVar, err)
	}
	defer func() {
		if err := os.Setenv(clientcmd.RecommendedConfigPathEnvVar, kubeconfig); err != nil {
			Logf("failed to export %s after scaling: %w", clientcmd.RecommendedConfigPathEnvVar, err)
		}
	}()

	if _, err := RunKubectl("default", "scale", "machinepool", "--replicas", strconv.Itoa(int(instanceCount)), vmssName); err != nil {
		return fmt.Errorf("failed to scale MachinePool %q: %w", vmssName, err)
	}
	return nil
}
