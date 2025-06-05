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

package nodemanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider"
	utilnode "sigs.k8s.io/cloud-provider-azure/pkg/util/node"
)

const (
	conditionType = "ScheduledEvent"
)

// EventChecker periodically checks for Azure scheduled events and updates the Kubernetes node status accordingly.
type EventChecker struct {
	nodeName   string
	kubeClient clientset.Interface
	recorder   record.EventRecorder
	imds       *provider.InstanceMetadataService

	updateFrequency time.Duration
}

// NewEventChecker creates a new EventChecker instance
func NewEventChecker(nodeName string, kubeClient clientset.Interface, metadataService *provider.InstanceMetadataService, updateFrequency time.Duration) *EventChecker {
	return &EventChecker{
		imds:            metadataService,
		nodeName:        nodeName,
		kubeClient:      kubeClient,
		updateFrequency: updateFrequency,
	}
}

// Run starts the event checker loop that periodically checks for Azure scheduled events
func (ec *EventChecker) Run(ctx context.Context) {
	ticker := time.NewTicker(ec.updateFrequency)
	defer ticker.Stop()

	klog.Infof("Starting Azure scheduled event checker for node %s with update frequency %s", ec.nodeName, ec.updateFrequency)
	for {
		select {
		case <-ctx.Done():
			klog.Infof("Stopping Azure scheduled event checker for node %s", ec.nodeName)
			return
		case <-ticker.C:
			if err := ec.CheckAzureScheduledEvents(context.Background()); err != nil {
				klog.Errorf("Failed to check Azure scheduled events: %v", err)
			}
		}
	}
}

// CheckAzureScheduledEvents queries the Azure metadata service for scheduled events
// and updates the Kubernetes node with a custom condition "NodeEvent".
func (ec *EventChecker) CheckAzureScheduledEvents(ctx context.Context) error {
	eventResponse, err := ec.imds.GetScheduledEvents()
	if err != nil {
		return err
	}

	node, err := ec.kubeClient.CoreV1().Nodes().Get(ctx, ec.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	currentCondition := ec.GetNodeEventCondition(node.Status.Conditions)
	if len(eventResponse.Events) == 0 {
		targetCondition := v1.NodeCondition{
			Type:    conditionType,
			Status:  v1.ConditionFalse,
			Reason:  "NoScheduledEvents",
			Message: "No scheduled events found",
		}
		if currentCondition == nil || targetCondition.Status != currentCondition.Status {
			klog.Infof("No scheduled events found for node %s, updating condition to %s", ec.nodeName, targetCondition.Status)
			return utilnode.SetNodeCondition(ec.kubeClient, types.NodeName(ec.nodeName), targetCondition)
		}

		return nil // No events to process, and no change in condition
	}

	targetCondition := ScheduledEventCondition(*eventResponse)
	if currentCondition == nil || targetCondition.Message != currentCondition.Message {
		klog.Infof("Scheduled events found for node %s, updating condition to %s", ec.nodeName, targetCondition.Status)
		return utilnode.SetNodeCondition(ec.kubeClient, types.NodeName(ec.nodeName), targetCondition)
	}
	return nil
}

func (ec *EventChecker) GetNodeEventCondition(conditions []v1.NodeCondition) *v1.NodeCondition {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return &condition
		}
	}

	return nil
}

func ScheduledEventCondition(event provider.EventResponse) v1.NodeCondition {
	nodeCondition := v1.NodeCondition{
		Type:               conditionType,
		Status:             v1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: time.Now()},
		Reason:             "ScheduledEventExists",
	}
	var messages []string
	for _, ev := range event.Events {
		messages = append(messages, fmt.Sprintf("EventID: %s, EventSource: %s, EventType: %s, NotBefore: %s, Duration: %d seconds, Description: %s", ev.EventID, ev.EventSource, ev.EventType, ev.NotBefore, ev.DurationInSeconds, ev.Description))
	}
	nodeCondition.Message = strings.Join(messages, "; ")
	return nodeCondition
}
