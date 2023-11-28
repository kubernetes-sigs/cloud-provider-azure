/*
Copyright 2020 The Kubernetes Authors.

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

package metrics

import (
	"bytes"
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"k8s.io/klog/v2"
)

type FakeLogger struct {
	logr.Logger
	infoBuffer  bytes.Buffer
	errorBuffer bytes.Buffer
}

func (logger *FakeLogger) Init(_ logr.RuntimeInfo) {}
func (logger *FakeLogger) Enabled(_ int) bool {
	return true
}
func (logger *FakeLogger) Info(_ int, _ string, keysAndValues ...interface{}) {
	// test result_code
	for i := 0; i < len(keysAndValues); i += 2 {
		var v interface{}
		k := keysAndValues[i]
		if i+1 < len(keysAndValues) {
			v = keysAndValues[i+1]
		} else {
			v = ""
		}
		if sK, ok := k.(string); ok && sK == "result_code" {
			logger.infoBuffer.WriteString(v.(string))
			break
		}
	}
}
func (logger *FakeLogger) Error(_ error, msg string, _ ...interface{}) {
	logger.errorBuffer.WriteString(msg)
}
func (logger *FakeLogger) WithValues(_ ...interface{}) logr.LogSink {
	return logger
}
func (logger *FakeLogger) WithName(_ string) logr.LogSink {
	return logger
}

var _ logr.LogSink = &FakeLogger{}

func init() {
	klog.InitFlags(nil)
}

func TestAzureMetricLabelCardinality(t *testing.T) {
	mc := NewMetricContext("test", "create", "resource_group", "subscription_id", "source")
	assert.Len(t, mc.attributes, len(metricLabels), "cardinalities of labels and values must match")
}

func TestAzureMetricLabelPrefix(t *testing.T) {
	mc := NewMetricContext("prefix", "request", "resource_group", "subscription_id", "source")
	found := false
	for _, attribute := range mc.attributes {
		if attribute == "prefix_request" {
			found = true
		}
	}
	assert.True(t, found, "request label must be prefixed")
}

func TestAzureMetricResultCode(t *testing.T) {
	err := flag.Set("v", "3")
	if err != nil {
		t.Fatalf("Failed to set verbosity in TestAzureMetricResultCode")
	}
	mc := NewMetricContext("prefix", "create_route", "resource_group", "subscription_id", "source")
	cases := []struct {
		isOperationSucceeded bool
		expectedResutCode    string
	}{
		{
			isOperationSucceeded: true,
			expectedResutCode:    "succeeded",
		},
		{
			isOperationSucceeded: false,
			expectedResutCode:    "failed_create_route",
		},
	}
	defer klog.ClearLogger()
	for _, tc := range cases {
		fakeLogger := &FakeLogger{}
		klog.SetLogger(logr.New(fakeLogger))
		mc.ObserveOperationWithResult(tc.isOperationSucceeded)
		assert.Equal(t, tc.expectedResutCode, fakeLogger.infoBuffer.String())
	}
}
