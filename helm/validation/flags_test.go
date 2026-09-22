//go:build helm

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

package validation_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cloud-provider/names"

	ccmapp "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app"
	ccmoptions "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/options"
	cnmapp "sigs.k8s.io/cloud-provider-azure/cmd/cloud-node-manager/app"
)

func TestChartFlags(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("Helm is required for chart flag validation: %v", err)
	}
	for minor := 21; minor <= currentKubeMinor(t); minor++ {
		for _, optional := range []bool{false, true} {
			t.Run(fmt.Sprintf("1.%d/optional=%t", minor, optional), func(t *testing.T) {
				args := []string{
					"template", "flag-contract", filepath.Join("..", "cloud-provider-azure"),
					"--kube-version", fmt.Sprintf("1.%d.0", minor),
				}
				if optional {
					args = append(args, "--values", filepath.Join("testdata", "optional-values.yaml"))
				}
				// The current release's image mapping is added separately until release images are published.
				if minor == currentKubeMinor(t) {
					args = append(args,
						"--set-string", fmt.Sprintf("cloudControllerManager.imageTag=v1.%d.0", minor),
						"--set-string", fmt.Sprintf("cloudNodeManager.imageTag=v1.%d.0", minor))
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, helm, args...)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				output, err := cmd.Output()
				if err != nil {
					t.Fatalf("helm template failed: %v\n%s", err, &stderr)
				}
				checkRenderedFlags(t, output)
			})
		}
	}
}

// currentKubeMinor returns the Kubernetes minor version this checkout builds
// against, derived from the k8s.io/cloud-provider dependency in go.mod, so the
// scenario range and current-release image handling track dependency bumps
// automatically.
func currentKubeMinor(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("Read go.mod: %v", err)
	}
	match := regexp.MustCompile(`(?m)^\s*k8s\.io/cloud-provider v0\.(\d+)\.`).FindSubmatch(data)
	if match == nil {
		t.Fatalf("Could not find k8s.io/cloud-provider version in go.mod")
	}
	minor, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("Parse k8s minor version %q: %v", match[1], err)
	}
	return minor
}

func checkRenderedFlags(t *testing.T, rendered []byte) {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	containers := make(map[string]corev1.Container)
	var script string
	for {
		var document struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"spec"`
			Data map[string]string `json:"data"`
		}
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Decode Helm output: %v", err)
		}
		if document.Kind == "ConfigMap" && document.Metadata.Name == "cloud-provider-azure-scripts" {
			script = document.Data["start.ps1"]
		}
		if document.Kind != "Deployment" && document.Kind != "DaemonSet" {
			continue
		}
		for _, container := range document.Spec.Template.Spec.Containers {
			if container.Name != "cloud-controller-manager" && container.Name != "cloud-node-manager" {
				continue
			}
			if _, exists := containers[document.Metadata.Name]; exists {
				t.Fatalf("Duplicate component in workload %s", document.Metadata.Name)
			}
			if container.Image == "" {
				t.Fatalf("Missing image for %s", document.Metadata.Name)
			}
			containers[document.Metadata.Name] = container
		}
	}

	for _, workload := range []string{"cloud-controller-manager", "cloud-node-manager", "cloud-node-manager-windows"} {
		container, ok := containers[workload]
		if !ok {
			t.Fatalf("Missing workload %s in rendered chart", workload)
		}
		args := container.Args
		if workload == "cloud-node-manager-windows" {
			if len(container.Command) != 1 || container.Command[0] != "powershell.exe" ||
				len(container.Args) != 1 || container.Args[0] != "$env:CONTAINER_SANDBOX_MOUNT_POINT/scripts/start.ps1" {
				t.Fatalf("%s: unsupported launcher command %v %v", workload, container.Command, container.Args)
			}
			var err error
			args, err = windowsArguments(script, container.Env)
			if err != nil {
				t.Fatalf("%s: %v", workload, err)
			}
		}
		if err := validateArguments(container.Name, args); err != nil {
			t.Errorf("%s: %v", workload, err)
		}
	}
}

func validateArguments(component string, args []string) error {
	var fs *pflag.FlagSet
	switch component {
	case "cloud-controller-manager":
		fs = ccmapp.NewCloudControllerManagerCommand().Flags()
	case "cloud-node-manager":
		fs = cnmapp.NewCloudNodeManagerCommand().Flags()
	default:
		return fmt.Errorf("unsupported component %q", component)
	}
	seen := make(map[string]bool)
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			return fmt.Errorf("expected --flag=value argument, got %q", arg)
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !hasValue {
			return fmt.Errorf("expected --flag=value argument, got %q", arg)
		}
		flag := fs.Lookup(name)
		if flag == nil {
			return fmt.Errorf("unknown flag --%s", name)
		}
		if seen[flag.Name] {
			return fmt.Errorf("duplicate flag --%s", flag.Name)
		}
		seen[flag.Name] = true
		if flag.Deprecated != "" || strings.Contains(strings.ToLower(flag.Usage), "deprecated") {
			return fmt.Errorf("deprecated flag --%s", name)
		}
		if flag.Hidden {
			return fmt.Errorf("hidden flag --%s", name)
		}
	}
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse %s flags: %w", component, err)
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if component != "cloud-controller-manager" {
		return nil
	}

	// Run the real options validation without starting controllers or contacting a cluster.
	options, err := ccmoptions.NewCloudControllerManagerOptions()
	if err != nil {
		return err
	}
	controllers := ccmapp.KnownControllers()
	disabled := ccmapp.ControllersDisabledByDefault.List()
	optionsFlags := pflag.NewFlagSet(component, pflag.ContinueOnError)
	for _, flags := range options.Flags(controllers, disabled).FlagSets {
		optionsFlags.AddFlagSet(flags)
	}
	fs.VisitAll(func(flag *pflag.Flag) {
		if optionsFlags.Lookup(flag.Name) == nil {
			optionsFlags.AddFlag(flag)
		}
	})
	optionsFlags.SetOutput(io.Discard)
	if err := optionsFlags.Parse(args); err != nil {
		return err
	}
	return options.Validate(controllers, disabled, names.CCMControllerAliases())
}

func windowsArguments(script string, env []corev1.EnvVar) ([]string, error) {
	// Recognize the chart's launcher explicitly; a changed script must not silently bypass validation.
	loops := regexp.MustCompile(`(?m)^\s*foreach \(\$var in @\(([^)]*)\)\) \{`).FindAllStringSubmatch(script, -1)
	launches := regexp.MustCompile(`(?m)^\s*Invoke-Expression "\$cm \$argList ([^"\r\n]+)"\s*$`).FindAllStringSubmatch(script, -1)
	if len(loops) != 1 || len(launches) != 1 {
		return nil, fmt.Errorf("unsupported Windows launcher: expected one environment loop and one CNM invocation")
	}
	values := make(map[string]string)
	for _, variable := range env {
		if _, exists := values[variable.Name]; exists {
			return nil, fmt.Errorf("duplicate Windows environment variable %s", variable.Name)
		}
		values[variable.Name] = variable.Value
	}
	var args []string
	consumed := make(map[string]bool)
	for _, entry := range strings.Split(loops[0][1], ",") {
		entry = strings.TrimSpace(entry)
		if len(entry) < 3 || entry[0] != '"' || entry[len(entry)-1] != '"' {
			return nil, fmt.Errorf("unsupported Windows environment entry %q", entry)
		}
		name := entry[1 : len(entry)-1]
		if consumed[name] {
			return nil, fmt.Errorf("duplicate Windows launcher environment entry %s", name)
		}
		consumed[name] = true
		if value, ok := values[name]; ok {
			if !strings.HasPrefix(value, "--") {
				return nil, fmt.Errorf("expected flag in Windows environment variable %s, got %q", name, value)
			}
			args = append(args, value)
		}
	}
	for name, value := range values {
		if strings.HasPrefix(value, "--") && !consumed[name] {
			return nil, fmt.Errorf("Windows flag environment variable %s is not consumed by the launcher", name)
		}
	}
	return append(args, strings.Fields(launches[0][1])...), nil
}

func TestFlagContractRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name      string
		component string
		args      []string
		want      string
	}{
		{"unknown", "cloud-controller-manager", []string{"--removed-flag=true"}, "unknown flag"},
		{"deprecated", "cloud-controller-manager", []string{"--node-sync-period=1s"}, "deprecated flag"},
		{"usage-only deprecation", "cloud-node-manager", []string{"--enable-deprecated-beta-topology-labels=false"}, "deprecated flag"},
		{"duplicate", "cloud-controller-manager", []string{"--leader-elect=true", "--leader-elect=false"}, "duplicate flag"},
		{"invalid value", "cloud-node-manager", []string{"--kube-api-burst=invalid"}, "parse cloud-node-manager flags"},
		{"removed lock type", "cloud-controller-manager", []string{"--cloud-config=azure.json", "--leader-elect-resource-lock=configmaps"}, `resourceLock value must be "leases"`},
		{"positional argument", "cloud-node-manager", []string{"unexpected"}, "expected --flag=value"},
		{"missing value", "cloud-node-manager", []string{"--wait-routes"}, "expected --flag=value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArguments(tc.component, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWindowsArguments(t *testing.T) {
	script := `foreach ($var in @("LOG_VERBOSITY")) {
}
Invoke-Expression "$cm $argList --node-name=$env:NODE_NAME --use-instance-metadata=true"`
	for _, tc := range []struct {
		name   string
		script string
		env    []corev1.EnvVar
		want   string
	}{
		{"missing launcher", "", nil, "unsupported Windows launcher"},
		{"unconsumed flag", script, []corev1.EnvVar{{Name: "EXTRA", Value: "--removed-flag=true"}}, "not consumed"},
		{"invalid environment value", script, []corev1.EnvVar{{Name: "LOG_VERBOSITY", Value: "invalid"}}, "expected flag"},
		{"duplicate environment variable", script, []corev1.EnvVar{{Name: "LOG_VERBOSITY"}, {Name: "LOG_VERBOSITY"}}, "duplicate Windows environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := windowsArguments(tc.script, tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	args, err := windowsArguments(script, []corev1.EnvVar{{Name: "LOG_VERBOSITY", Value: "--v=2"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "--v=2 --node-name=$env:NODE_NAME --use-instance-metadata=true" {
		t.Fatalf("unexpected Windows arguments: %s", got)
	}
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"--removed-flag=true", "unknown flag"},
		{"--enable-deprecated-beta-topology-labels=true", "deprecated flag"},
		{"--use-instance-metadata=false", "duplicate flag"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			args, err := windowsArguments(script, []corev1.EnvVar{{Name: "LOG_VERBOSITY", Value: tc.value}})
			if err != nil {
				t.Fatal(err)
			}
			err = validateArguments("cloud-node-manager", args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
