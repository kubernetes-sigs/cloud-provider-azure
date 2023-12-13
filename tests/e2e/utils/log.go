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
	"fmt"
	"runtime"
	"time"

	"github.com/onsi/ginkgo/v2"
)

func nowStamp() string {
	return time.Now().Format(time.StampMilli)
}

func log(level string, format string, args ...interface{}) {
	_, file, line, _ := runtime.Caller(2)
	extra := fmt.Sprintf(" [%s:%d]", file, line)

	fmt.Fprintf(ginkgo.GinkgoWriter, nowStamp()+": "+level+": "+format+extra+"\n", args...)
}

// Logf prints info logs
func Logf(format string, args ...interface{}) {
	log("INFO", format, args...)
}

func PrintCreateSVCSuccessfully(svc string, ns string) {
	Logf("Successfully created LoadBalancer service %q in namespace %q", svc, ns)
}
