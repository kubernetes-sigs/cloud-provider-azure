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

// test suite labels
const (
	TestSuiteLabelFeatureAutoscaling = "Feature:Autoscaling"
	TestSuiteLabelSerial             = "Serial"
	TestSuiteLabelSlow               = "Slow"
	TestSuiteLabelMultiNodePools     = "Multi-Nodepool"
	TestSuiteLabelSingleNodePool     = "Single-Nodepool"
	TestSuiteLabelVMSS               = "VMSS"
	TestSuiteLabelVMSSScale          = "VMSS-Scale"
	TestSuiteLabelSpotVM             = "Spot-VM"
	TestSuiteLabelKubenet            = "Kubenet"
	TestSuiteLabelMultiGroup         = "Multi-Group"
	TestSuiteLabelAvailabilitySet    = "AvailabilitySet"
	TestSuiteLabelPrivateLinkService = "PLS"
	TestSuiteLabelSLBOutbound        = "SLBOutbound"
	TestSuiteLabelServiceAnnotation  = "ServiceAnnotation"
	TestSuiteLabelCredential         = "Credential"
	TestSuiteLabelNode               = "Node"
	TestSuiteLabelLB                 = "LB"
	TestSuiteLabelMultiPorts         = "Multi-Ports"
	TestSuiteLabelNSG                = "NSG"
	TestSuiteLabelNonMultiSLB        = "Non-Multi-Slb"
	TestSuiteLabelMultiSLB           = "Multi-SLB"
	TestSuiteUnmanagedNode           = "Unmanaged-Node"
	TestSuiteLabelSharedHealthProbe  = "Shared-Health-Probe"

	// If "TEST_CCM" is true, the test is running on a CAPZ cluster.
	CAPZTestCCM = "TEST_CCM"
	// If "E2E_ON_AKS_CLUSTER" is true, the test is running on a AKS cluster.
	AKSTestCCM     = "E2E_ON_AKS_CLUSTER"
	AKSClusterType = "CLUSTER_TYPE"
	// If "INGEST_TEST_RESULT" is true, the test result needs ingestion to kusto
	IngestTestResult = "INGEST_TEST_RESULT"
	// LB backendpool config type, may be nodeIP
	LBBackendPoolConfigType = "LB_BACKEND_POOL_CONFIG_TYPE"

	TrueValue = "true"
)
