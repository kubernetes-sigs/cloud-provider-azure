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

package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routetableclient/mockroutetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestDeleteRoute(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	routeTableClient := mockroutetableclient.NewMockInterface(ctrl)

	cloud := &Cloud{
		RouteTablesClient: routeTableClient,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:     utilsets.NewString(),
		nodeInformerSynced: func() bool { return true },
	}
	cache, _ := cloud.newRouteTableCache()
	cloud.rtCache = cache
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())
	route := cloudprovider.Route{
		TargetNode:      "node",
		DestinationCIDR: "1.2.3.4/24",
	}
	routeName := mapNodeNameToRouteName(false, route.TargetNode, route.DestinationCIDR)
	routeTables := network.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Routes: &[]network.Route{
				{
					Name: &routeName,
				},
			},
		},
	}
	routeTablesAfterDeletion := network.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Routes: &[]network.Route{},
		},
	}
	routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, "").Return(routeTables, nil).AnyTimes()
	routeTableClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, routeTablesAfterDeletion, "").Return(nil)
	err := cloud.DeleteRoute(context.TODO(), "cluster", &route)
	if err != nil {
		t.Errorf("unexpected error deleting route: %v", err)
		t.FailNow()
	}

	// test delete route for unmanaged nodes.
	nodeName := "node1"
	nodeCIDR := "4.3.2.1/24"
	cloud.unmanagedNodes.Insert(nodeName)
	cloud.routeCIDRs = map[string]string{
		nodeName: nodeCIDR,
	}
	route1 := cloudprovider.Route{
		TargetNode:      MapRouteNameToNodeName(false, nodeName),
		DestinationCIDR: nodeCIDR,
	}
	err = cloud.DeleteRoute(context.TODO(), "cluster", &route1)
	if err != nil {
		t.Errorf("unexpected error deleting route: %v", err)
		t.FailNow()
	}
	cidr, found := cloud.routeCIDRs[nodeName]
	if found {
		t.Errorf("unexpected CIDR item (%q) for %s", cidr, nodeName)
	}
}

func TestDeleteRouteDualStack(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	routeTableClient := mockroutetableclient.NewMockInterface(ctrl)

	cloud := &Cloud{
		RouteTablesClient: routeTableClient,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:       utilsets.NewString(),
		nodeInformerSynced:   func() bool { return true },
		ipv6DualStackEnabled: true,
	}
	cache, _ := cloud.newRouteTableCache()
	cloud.rtCache = cache
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())

	route := cloudprovider.Route{
		TargetNode:      "node",
		DestinationCIDR: "1.2.3.4/24",
	}
	routeName := mapNodeNameToRouteName(true, route.TargetNode, route.DestinationCIDR)
	routeNameIPV4 := mapNodeNameToRouteName(false, route.TargetNode, route.DestinationCIDR)
	routeTables := network.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Routes: &[]network.Route{
				{
					Name: &routeName,
				},
				{
					Name: &routeNameIPV4,
				},
			},
		},
	}
	routeTablesAfterFirstDeletion := network.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Routes: &[]network.Route{
				{
					Name: &routeNameIPV4,
				},
			},
		},
	}
	routeTablesAfterSecondDeletion := network.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Routes: &[]network.Route{},
		},
	}
	routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, "").Return(routeTables, nil)
	routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, "").Return(routeTablesAfterFirstDeletion, nil)
	routeTableClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, routeTablesAfterFirstDeletion, "").Return(nil)
	routeTableClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, routeTablesAfterSecondDeletion, "").Return(nil)
	err := cloud.DeleteRoute(context.TODO(), "cluster", &route)
	if err != nil {
		t.Errorf("unexpected error deleting route: %v", err)
		t.FailNow()
	}

}

func TestCreateRoute(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	routeTableClient := mockroutetableclient.NewMockInterface(ctrl)
	mockVMSet := NewMockVMSet(ctrl)

	cloud := &Cloud{
		RouteTablesClient: routeTableClient,
		VMSet:             mockVMSet,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:     utilsets.NewString(),
		nodeInformerSynced: func() bool { return true },
	}
	cache, _ := cloud.newRouteTableCache()
	cloud.rtCache = cache
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())

	route := cloudprovider.Route{TargetNode: "node", DestinationCIDR: "1.2.3.4/24"}
	nodePrivateIP := "2.4.6.8"
	networkRoute := &[]network.Route{
		{
			Name: ptr.To("node"),
			RoutePropertiesFormat: &network.RoutePropertiesFormat{
				AddressPrefix:    ptr.To("1.2.3.4/24"),
				NextHopIPAddress: &nodePrivateIP,
				NextHopType:      network.RouteNextHopTypeVirtualAppliance,
			},
		},
	}

	testCases := []struct {
		name                  string
		routeTableName        string
		initialRoute          *[]network.Route
		updatedRoute          *[]network.Route
		hasUnmanagedNodes     bool
		nodeInformerNotSynced bool
		ipv6DualStackEnabled  bool
		routeTableNotExist    bool
		unmanagedNodeName     string
		routeCIDRs            map[string]string
		expectedRouteCIDRs    map[string]string

		getIPError        error
		getErr            *retry.Error
		secondGetErr      *retry.Error
		createOrUpdateErr *retry.Error
		expectedErrMsg    error
	}{
		{
			name:           "CreateRoute should report an error if route table name is not configured",
			routeTableName: "",
			expectedErrMsg: fmt.Errorf("route table name is not configured"),
		},
		{
			name:           "CreateRoute should create route if route doesn't exist",
			routeTableName: "rt1",
			updatedRoute:   networkRoute,
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke CreateOrUpdateRouteTable",
			routeTableName: "rt2",
			updatedRoute:   networkRoute,
			createOrUpdateErr: &retry.Error{
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("CreateOrUpdate error"),
			},
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: CreateOrUpdate error"),
		},
		{
			name:           "CreateRoute should do nothing if route already exists",
			routeTableName: "rt3",
			initialRoute:   networkRoute,
			updatedRoute:   networkRoute,
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke createRouteTable",
			routeTableName: "rt4",
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			createOrUpdateErr: &retry.Error{
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("CreateOrUpdate error"),
			},
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: CreateOrUpdate error"),
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke getRouteTable for the second time",
			routeTableName: "rt5",
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			secondGetErr: &retry.Error{
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("Get error"),
			},
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: Get error"),
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke routeTableClient.Get",
			routeTableName: "rt6",
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("Get error"),
			},
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: Get error"),
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke GetIPByNodeName",
			routeTableName: "rt7",
			getIPError:     fmt.Errorf("getIP error"),
			expectedErrMsg: fmt.Errorf("timed out waiting for the condition"),
		},
		{
			name:               "CreateRoute should add route to cloud.RouteCIDRs if node is unmanaged",
			routeTableName:     "rt8",
			hasUnmanagedNodes:  true,
			unmanagedNodeName:  "node",
			routeCIDRs:         map[string]string{},
			expectedRouteCIDRs: map[string]string{"node": "1.2.3.4/24"},
		},
		{
			name:           "CreateRoute should create route if cloud.ipv6DualStackEnabled is true and route doesn't exist",
			routeTableName: "rt9",
			updatedRoute: &[]network.Route{
				{
					Name: ptr.To("node____123424"),
					RoutePropertiesFormat: &network.RoutePropertiesFormat{
						AddressPrefix:    ptr.To("1.2.3.4/24"),
						NextHopIPAddress: &nodePrivateIP,
						NextHopType:      network.RouteNextHopTypeVirtualAppliance,
					},
				},
			},
			ipv6DualStackEnabled: true,
		},
		{
			name:                  "CreateRoute should report error if node informer is not synced",
			nodeInformerNotSynced: true,
			expectedErrMsg:        fmt.Errorf("node informer is not synced when trying to GetUnmanagedNodes"),
		},
	}

	for _, test := range testCases {
		initialTable := network.RouteTable{
			Name:     ptr.To(test.routeTableName),
			Location: &cloud.Location,
			RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
				Routes: test.initialRoute,
			},
		}
		updatedTable := network.RouteTable{
			Name:     ptr.To(test.routeTableName),
			Location: &cloud.Location,
			RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
				Routes: test.updatedRoute,
			},
		}

		cloud.RouteTableName = test.routeTableName
		cloud.ipv6DualStackEnabled = test.ipv6DualStackEnabled
		if test.hasUnmanagedNodes {
			cloud.unmanagedNodes.Insert(test.unmanagedNodeName)
			cloud.routeCIDRs = test.routeCIDRs
		} else {
			cloud.unmanagedNodes = utilsets.NewString()
			cloud.routeCIDRs = nil
		}
		if test.nodeInformerNotSynced {
			cloud.nodeInformerSynced = func() bool { return false }
		} else {
			cloud.nodeInformerSynced = func() bool { return true }
		}

		mockVMSet.EXPECT().GetIPByNodeName(gomock.Any(), gomock.Any()).Return(nodePrivateIP, "", test.getIPError).MaxTimes(1)
		mockVMSet.EXPECT().GetPrivateIPsByNodeName(gomock.Any(), "node").Return([]string{nodePrivateIP, "10.10.10.10"}, nil).MaxTimes(1)
		routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, "").Return(initialTable, test.getErr).MaxTimes(1)
		routeTableClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, updatedTable, "").Return(test.createOrUpdateErr).MaxTimes(1)

		//Here is the second invocation when route table doesn't exist
		routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, "").Return(initialTable, test.secondGetErr).MaxTimes(1)

		err := cloud.CreateRoute(context.TODO(), "cluster", "unused", &route)
		assert.Equal(t, cloud.routeCIDRs, test.expectedRouteCIDRs, test.name)
		if err != nil {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
		}
	}
}

func TestCreateRouteTable(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	routeTableClient := mockroutetableclient.NewMockInterface(ctrl)

	cloud := &Cloud{
		RouteTablesClient: routeTableClient,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
	}
	cache, _ := cloud.newRouteTableCache()
	cloud.rtCache = cache

	expectedTable := network.RouteTable{
		Name:                       &cloud.RouteTableName,
		Location:                   &cloud.Location,
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{},
	}
	routeTableClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.RouteTableResourceGroup, cloud.RouteTableName, expectedTable, "").Return(nil)
	err := cloud.createRouteTable()
	if err != nil {
		t.Errorf("unexpected error in creating route table: %v", err)
		t.FailNow()
	}
}

func TestProcessRoutes(t *testing.T) {
	tests := []struct {
		rt            network.RouteTable
		expectedRoute []cloudprovider.Route
		exists        bool
		expectErr     bool
		name          string
		expectedError string
		err           error
	}{
		{
			err:           fmt.Errorf("test error"),
			expectErr:     true,
			expectedError: "test error",
		},
		{
			exists: false,
			name:   "doesn't exist",
		},
		{
			rt:     network.RouteTable{},
			exists: true,
			name:   "nil routes",
		},
		{
			rt: network.RouteTable{
				RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{},
			},
			exists: true,
			name:   "no routes",
		},
		{
			rt: network.RouteTable{
				RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
					Routes: &[]network.Route{
						{
							Name: ptr.To("name"),
							RoutePropertiesFormat: &network.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/16"),
							},
						},
					},
				},
			},
			exists: true,
			expectedRoute: []cloudprovider.Route{
				{
					Name:            "name",
					TargetNode:      MapRouteNameToNodeName(false, "name"),
					DestinationCIDR: "1.2.3.4/16",
				},
			},
			name: "one route",
		},
		{
			rt: network.RouteTable{
				RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
					Routes: &[]network.Route{
						{
							Name: ptr.To("name"),
							RoutePropertiesFormat: &network.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/16"),
							},
						},
						{
							Name: ptr.To("name2"),
							RoutePropertiesFormat: &network.RoutePropertiesFormat{
								AddressPrefix: ptr.To("5.6.7.8/16"),
							},
						},
					},
				},
			},
			exists: true,
			expectedRoute: []cloudprovider.Route{
				{
					Name:            "name",
					TargetNode:      MapRouteNameToNodeName(false, "name"),
					DestinationCIDR: "1.2.3.4/16",
				},
				{
					Name:            "name2",
					TargetNode:      MapRouteNameToNodeName(false, "name2"),
					DestinationCIDR: "5.6.7.8/16",
				},
			},
			name: "more routes",
		},
	}
	for _, test := range tests {
		routes, err := processRoutes(false, test.rt, test.exists, test.err)
		if test.expectErr {
			if err == nil {
				t.Errorf("%s: unexpected non-error", test.name)
				continue
			}
			if err.Error() != test.expectedError {
				t.Errorf("%s: Expected error: %v, saw error: %v", test.name, test.expectedError, err.Error())
				continue
			}
		}
		if !test.expectErr && err != nil {
			t.Errorf("%s; unexpected error: %v", test.name, err)
			continue
		}
		if len(routes) != len(test.expectedRoute) {
			t.Errorf("%s: Unexpected difference: %#v vs %#v", test.name, routes, test.expectedRoute)
			continue
		}
		for ix := range test.expectedRoute {
			if !reflect.DeepEqual(test.expectedRoute[ix], *routes[ix]) {
				t.Errorf("%s: Unexpected difference: %#v vs %#v", test.name, test.expectedRoute[ix], *routes[ix])
			}
		}
	}
}

func TestFindFirstIPByFamily(t *testing.T) {
	firstIPv4 := "10.0.0.1"
	firstIPv6 := "2001:1234:5678:9abc::9"
	testIPs := []string{
		firstIPv4,
		"11.0.0.1",
		firstIPv6,
		"fda4:6dee:effc:62a0:0:0:0:0",
	}
	testCases := []struct {
		ipv6           bool
		ips            []string
		expectedIP     string
		expectedErrMsg error
	}{
		{
			ipv6:       true,
			ips:        testIPs,
			expectedIP: firstIPv6,
		},
		{
			ipv6:       false,
			ips:        testIPs,
			expectedIP: firstIPv4,
		},
		{
			ipv6:           true,
			ips:            []string{"10.0.0.1"},
			expectedErrMsg: fmt.Errorf("no match found matching the ipfamily requested"),
		},
	}
	for _, test := range testCases {
		ip, err := findFirstIPByFamily(test.ips, test.ipv6)
		assert.Equal(t, test.expectedErrMsg, err)
		assert.Equal(t, test.expectedIP, ip)
	}
}

func TestRouteNameFuncs(t *testing.T) {
	v4CIDR := "10.0.0.1/16"
	v6CIDR := "fd3e:5f02:6ec0:30ba::/64"
	nodeName := "thisNode"
	testCases := []struct {
		ipv6DualStackEnabled bool
	}{
		{
			ipv6DualStackEnabled: true,
		},
		{
			ipv6DualStackEnabled: false,
		},
	}
	for _, test := range testCases {
		routeName := mapNodeNameToRouteName(test.ipv6DualStackEnabled, types.NodeName(nodeName), v4CIDR)
		outNodeName := MapRouteNameToNodeName(test.ipv6DualStackEnabled, routeName)
		assert.Equal(t, string(outNodeName), nodeName)

		routeName = mapNodeNameToRouteName(test.ipv6DualStackEnabled, types.NodeName(nodeName), v6CIDR)
		outNodeName = MapRouteNameToNodeName(test.ipv6DualStackEnabled, routeName)
		assert.Equal(t, string(outNodeName), nodeName)
	}
}

func TestListRoutes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	routeTableClient := mockroutetableclient.NewMockInterface(ctrl)
	mockVMSet := NewMockVMSet(ctrl)

	cloud := &Cloud{
		RouteTablesClient: routeTableClient,
		VMSet:             mockVMSet,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:     utilsets.NewString(),
		nodeInformerSynced: func() bool { return true },
	}
	cache, _ := cloud.newRouteTableCache()
	cloud.rtCache = cache
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())

	testCases := []struct {
		name                  string
		routeTableName        string
		routeTable            network.RouteTable
		hasUnmanagedNodes     bool
		nodeInformerNotSynced bool
		unmanagedNodeName     string
		routeCIDRs            map[string]string
		expectedRoutes        []*cloudprovider.Route
		getErr                *retry.Error
		expectedErrMsg        error
	}{
		{
			name:           "ListRoutes should return correct routes",
			routeTableName: "rt1",
			routeTable: network.RouteTable{
				Name:     ptr.To("rt1"),
				Location: &cloud.Location,
				RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
					Routes: &[]network.Route{
						{
							Name: ptr.To("node"),
							RoutePropertiesFormat: &network.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/24"),
							},
						},
					},
				},
			},
			expectedRoutes: []*cloudprovider.Route{
				{
					Name:            "node",
					TargetNode:      MapRouteNameToNodeName(false, "node"),
					DestinationCIDR: "1.2.3.4/24",
				},
			},
		},
		{
			name:              "ListRoutes should return correct routes if there's unmanaged nodes",
			routeTableName:    "rt2",
			hasUnmanagedNodes: true,
			unmanagedNodeName: "unmanaged-node",
			routeCIDRs:        map[string]string{"unmanaged-node": "2.2.3.4/24"},
			routeTable: network.RouteTable{
				Name:     ptr.To("rt2"),
				Location: &cloud.Location,
				RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
					Routes: &[]network.Route{
						{
							Name: ptr.To("node"),
							RoutePropertiesFormat: &network.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/24"),
							},
						},
					},
				},
			},
			expectedRoutes: []*cloudprovider.Route{
				{
					Name:            "node",
					TargetNode:      MapRouteNameToNodeName(false, "node"),
					DestinationCIDR: "1.2.3.4/24",
				},
				{
					Name:            "unmanaged-node",
					TargetNode:      MapRouteNameToNodeName(false, "unmanaged-node"),
					DestinationCIDR: "2.2.3.4/24",
				},
			},
		},
		{
			name:           "ListRoutes should return nil if routeTable don't exist",
			routeTableName: "rt3",
			routeTable:     network.RouteTable{},
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			expectedRoutes: []*cloudprovider.Route{},
		},
		{
			name:           "ListRoutes should report error if error occurs when invoke routeTableClient.Get",
			routeTableName: "rt4",
			routeTable:     network.RouteTable{},
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("Get error"),
			},
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: Get error"),
		},
		{
			name:                  "ListRoutes should report error if node informer is not synced",
			routeTableName:        "rt5",
			nodeInformerNotSynced: true,
			routeTable:            network.RouteTable{},
			expectedErrMsg:        fmt.Errorf("node informer is not synced when trying to GetUnmanagedNodes"),
		},
	}

	for _, test := range testCases {
		if test.hasUnmanagedNodes {
			cloud.unmanagedNodes.Insert(test.unmanagedNodeName)
			cloud.routeCIDRs = test.routeCIDRs
		} else {
			cloud.unmanagedNodes = utilsets.NewString()
			cloud.routeCIDRs = nil
		}

		if test.nodeInformerNotSynced {
			cloud.nodeInformerSynced = func() bool { return false }
		} else {
			cloud.nodeInformerSynced = func() bool { return true }
		}

		cloud.RouteTableName = test.routeTableName
		routeTableClient.EXPECT().Get(gomock.Any(), cloud.RouteTableResourceGroup, test.routeTableName, "").Return(test.routeTable, test.getErr)

		routes, err := cloud.ListRoutes(context.TODO(), "cluster")
		assert.Equal(t, test.expectedRoutes, routes, test.name)
		if test.expectedErrMsg != nil {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
		}
	}
}

func TestCleanupOutdatedRoutes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, testCase := range []struct {
		description                          string
		existingRoutes, expectedRoutes       []network.Route
		existingNodeNames                    *utilsets.IgnoreCaseSet
		expectedChanged, enableIPV6DualStack bool
	}{
		{
			description: "cleanupOutdatedRoutes should delete outdated non-dualstack routes when dualstack is enabled",
			existingRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
			},
			existingNodeNames:   utilsets.NewString("aks-node1-vmss000000"),
			enableIPV6DualStack: true,
			expectedChanged:     true,
		},
		{
			description: "cleanupOutdatedRoutes should delete outdated dualstack routes when dualstack is disabled",
			existingRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			existingNodeNames: utilsets.NewString("aks-node1-vmss000000"),
			expectedChanged:   true,
		},
		{
			description: "cleanupOutdatedRoutes should not delete unmanaged routes when dualstack is enabled",
			existingRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			existingNodeNames:   utilsets.NewString("aks-node1-vmss000001"),
			enableIPV6DualStack: true,
		},
		{
			description: "cleanupOutdatedRoutes should not delete unmanaged routes when dualstack is disabled",
			existingRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []network.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			existingNodeNames: utilsets.NewString("aks-node1-vmss000001"),
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cloud := &Cloud{
				ipv6DualStackEnabled: testCase.enableIPV6DualStack,
				nodeNames:            testCase.existingNodeNames,
			}

			d := &delayedRouteUpdater{
				az: cloud,
			}

			routes, changed := d.cleanupOutdatedRoutes(testCase.existingRoutes)
			assert.Equal(t, testCase.expectedChanged, changed)
			assert.Equal(t, testCase.expectedRoutes, routes)
		})
	}
}

func TestEnsureRouteTableTagged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.Tags = "a=b,c=d"

	expectedTags := map[string]*string{
		"a": ptr.To("b"),
		"c": ptr.To("d"),
	}
	rt := &network.RouteTable{}
	tags, changed := cloud.ensureRouteTableTagged(rt)
	assert.Equal(t, expectedTags, tags)
	assert.True(t, changed)

	cloud.RouteTableResourceGroup = "rg1"
	rt = &network.RouteTable{}
	tags, changed = cloud.ensureRouteTableTagged(rt)
	assert.Nil(t, tags)
	assert.False(t, changed)
}
