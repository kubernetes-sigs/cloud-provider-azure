/*

Copyright 2016 The Kubernetes Authors.

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

package probes

import (
	"net/http"

	"k8s.io/klog"
)

var defaultLivenessProbePort = "8085"
var livenessProbePortFlagName = "liveness-probe-port"
var livenessProbePort string

func initHealthProbe() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		klog.V(8).Info("Got a health check")
		w.WriteHeader(200)
	})
}

func startAsync(port string) {
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		klog.Errorf("Http listen and serve error: %+v", err)
		panic(err)
	} else {
		klog.Info("Http listen and serve started !")
	}
}

//Start - Starts the required http server to start the probe to respond.
func Start(port string) {
	klog.Infof("/healthz activated on port: %s", port)
	go startAsync(port)
}

// InitAndStart - Initialize the default probes and starts the http listening port.
func InitAndStart() {
	initHealthProbe()
	klog.V(3).Info("Initialized health probe")
	// TODO: Find a way to extract the liveness-probe-port parameter from cobra commandline.
	livenessProbePort = defaultLivenessProbePort
	// start the probe.
	Start(livenessProbePort)
	klog.V(3).Info("Started health probe")
}
