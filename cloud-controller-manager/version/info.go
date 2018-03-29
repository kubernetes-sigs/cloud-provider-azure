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

package version

import (
	"fmt"
	"runtime"
)

var (
	version   string
	buildDate string
)

type info struct {
	Version   string
	BuildDate string
	GoVersion string
}

// String returns info as a human-friendly version string.
func (v info) String() string {
	return fmt.Sprintf("%#v", v)
}

func getInfo() info {
	// These variables typically come from -ldflags settings and in
	// their absence fallback to the settings in pkg/version/version.go
	return info{
		Version:   version,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
	}
}
