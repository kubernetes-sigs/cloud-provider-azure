/*
Copyright 2023 The Kubernetes Authors.

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

package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/pires/go-proxyproto"

	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	var healthCheckPort, targetPort int
	flag.IntVar(&healthCheckPort, "health-check-port", 10356, "Port number for the health check service that exposes to the user and will be forwarded to the targetPort.")
	flag.IntVar(&targetPort, "target-port", 10256, "Port number that receives the forwarded traffic from the health check port, and will be listened by the kube-proxy.")
	flag.Parse()

	targetUrl, _ := url.Parse(fmt.Sprintf("http://localhost:%s", strconv.Itoa(targetPort)))

	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	klog.Infof("target url: %s", targetUrl)

	http.Handle("/", proxy)
	klog.Infof("proxying from port %d to port %d", healthCheckPort, targetPort)

	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%s", strconv.Itoa(healthCheckPort)))
	if err != nil {
		klog.Errorf("failed to listen on port %d: %s", targetPort, err)
		panic(err)
	}
	klog.Infof("listening on port %d", healthCheckPort)

	proxyListener := &proxyproto.Listener{Listener: listener}
	defer func(proxyListener *proxyproto.Listener) {
		err := proxyListener.Close()
		if err != nil {
			klog.Errorf("failed to close proxy listener: %s", err)
			panic(err)
		}
	}(proxyListener)

	klog.Infof("listening on port with proxy listener %d", healthCheckPort)
	err = http.Serve(proxyListener, nil)
	if err != nil {
		klog.Errorf("failed to serve: %s", err)
		panic(err)
	}
}
