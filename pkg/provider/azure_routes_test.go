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
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/routetable"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestDeleteRoute(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRTRepo := routetable.NewMockRepository(ctrl)
	cloud := &Cloud{
		routeTableRepo: mockRTRepo,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:     utilsets.NewString(),
		nodeInformerSynced: func() bool { return true },
	}
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())
	route := cloudprovider.Route{
		TargetNode:      "node",
		DestinationCIDR: "1.2.3.4/24",
	}
	routeName := mapNodeNameToRouteName(false, route.TargetNode, route.DestinationCIDR)
	routeTables := &armnetwork.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			Routes: []*armnetwork.Route{
				{
					Name: &routeName,
				},
			},
		},
	}
	routeTablesAfterDeletion := armnetwork.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			Routes: []*armnetwork.Route{},
		},
	}

	mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(routeTables, nil).AnyTimes()
	mockRTRepo.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, routeTable armnetwork.RouteTable) (*armnetwork.RouteTable, error) {
			assert.Equal(t, routeTablesAfterDeletion, routeTable)
			return nil, nil
		},
	).AnyTimes()

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
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRTRepo := routetable.NewMockRepository(ctrl)
	cloud := &Cloud{
		routeTableRepo: mockRTRepo,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:       utilsets.NewString(),
		nodeInformerSynced:   func() bool { return true },
		ipv6DualStackEnabled: true,
	}
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())

	route := cloudprovider.Route{
		TargetNode:      "node",
		DestinationCIDR: "1.2.3.4/24",
	}
	routeName := mapNodeNameToRouteName(true, route.TargetNode, route.DestinationCIDR)
	routeNameIPV4 := mapNodeNameToRouteName(false, route.TargetNode, route.DestinationCIDR)
	routeTables := &armnetwork.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			Routes: []*armnetwork.Route{
				{
					Name: &routeName,
				},
				{
					Name: &routeNameIPV4,
				},
			},
		},
	}
	routeTablesAfterFirstDeletion := armnetwork.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			Routes: []*armnetwork.Route{
				{
					Name: &routeNameIPV4,
				},
			},
		},
	}
	routeTablesAfterSecondDeletion := armnetwork.RouteTable{
		Name:     &cloud.RouteTableName,
		Location: &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			Routes: []*armnetwork.Route{},
		},
	}
	mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(routeTables, nil)
	mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(&routeTablesAfterFirstDeletion, nil)
	mockRTRepo.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, routeTable armnetwork.RouteTable) (*armnetwork.RouteTable, error) {
			assert.Equal(t, routeTablesAfterFirstDeletion, routeTable)
			return nil, nil
		},
	)
	mockRTRepo.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, routeTable armnetwork.RouteTable) (*armnetwork.RouteTable, error) {
			assert.Equal(t, routeTablesAfterSecondDeletion, routeTable)
			return nil, nil
		},
	)
	err := cloud.DeleteRoute(context.TODO(), "cluster", &route)
	if err != nil {
		t.Errorf("unexpected error deleting route: %v", err)
		t.FailNow()
	}

}

func TestCreateRoute(t *testing.T) {
	t.Parallel()

	route := cloudprovider.Route{TargetNode: "node", DestinationCIDR: "1.2.3.4/24"}
	nodePrivateIP := "2.4.6.8"
	networkRoute := []*armnetwork.Route{
		{
			Name: ptr.To("node"),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    ptr.To("1.2.3.4/24"),
				NextHopIPAddress: &nodePrivateIP,
				NextHopType:      ptr.To(armnetwork.RouteNextHopTypeVirtualAppliance),
			},
		},
	}

	tests := []struct {
		name                  string
		routeTableName        string
		initialRoute          []*armnetwork.Route
		updatedRoute          []*armnetwork.Route
		hasUnmanagedNodes     bool
		nodeInformerNotSynced bool
		ipv6DualStackEnabled  bool
		routeTableNotExist    bool
		unmanagedNodeName     string
		routeCIDRs            map[string]string
		expectedRouteCIDRs    map[string]string

		getIPError        error
		firstGetNotFound  bool // FIXME: it's very confusing; should refactor the whole test
		getErr            error
		secondGetErr      error
		createOrUpdateErr error
		expectedErrMsg    error
	}{
		{
			name:           "CreateRoute should create route if route doesn't exist",
			routeTableName: "rt1",
			updatedRoute:   networkRoute,
		},
		{
			name:              "CreateRoute should report error if error occurs when invoke CreateOrUpdateRouteTable",
			routeTableName:    "rt2",
			updatedRoute:      networkRoute,
			createOrUpdateErr: errors.New("dummy"),
			expectedErrMsg:    errors.New("dummy"),
		},
		{
			name:           "CreateRoute should do nothing if route already exists",
			routeTableName: "rt3",
			initialRoute:   networkRoute,
			updatedRoute:   networkRoute,
		},
		{
			name:              "CreateRoute should report error if error occurs when invoke createRouteTable",
			routeTableName:    "rt4",
			firstGetNotFound:  true,
			createOrUpdateErr: errors.New("create or update dummy"),
			expectedErrMsg:    errors.New("create or update dummy"),
		},
		{
			name:             "CreateRoute should report error if error occurs when invoke getRouteTable for the second time",
			routeTableName:   "rt5",
			firstGetNotFound: true,
			secondGetErr:     errors.New("second get dummy"),
			expectedErrMsg:   errors.New("second get dummy"),
		},
		{
			name:           "CreateRoute should report error if error occurs when invoke routeTableClient.Get",
			routeTableName: "rt6",
			getErr:         errors.New("first get dummy"),
			expectedErrMsg: errors.New("first get dummy"),
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
			updatedRoute: []*armnetwork.Route{
				{
					Name: ptr.To("node____123424"),
					Properties: &armnetwork.RoutePropertiesFormat{
						AddressPrefix:    ptr.To("1.2.3.4/24"),
						NextHopIPAddress: &nodePrivateIP,
						NextHopType:      ptr.To(armnetwork.RouteNextHopTypeVirtualAppliance),
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockVMSet := NewMockVMSet(ctrl)

			mockRTRepo := routetable.NewMockRepository(ctrl)
			cloud := &Cloud{
				routeTableRepo: mockRTRepo,
				VMSet:          mockVMSet,
				Config: Config{
					RouteTableResourceGroup: "foo",
					RouteTableName:          "bar",
					Location:                "location",
				},
				unmanagedNodes:     utilsets.NewString(),
				nodeInformerSynced: func() bool { return true },
			}
			cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
			go cloud.routeUpdater.run(context.Background())

			initialTable := &armnetwork.RouteTable{
				Name:     ptr.To(tt.routeTableName),
				Location: &cloud.Location,
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: tt.initialRoute,
				},
			}
			updatedTable := armnetwork.RouteTable{
				Name:     ptr.To(tt.routeTableName),
				Location: &cloud.Location,
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: tt.updatedRoute,
				},
			}

			cloud.RouteTableName = tt.routeTableName
			cloud.ipv6DualStackEnabled = tt.ipv6DualStackEnabled
			if tt.hasUnmanagedNodes {
				cloud.unmanagedNodes.Insert(tt.unmanagedNodeName)
				cloud.routeCIDRs = tt.routeCIDRs
			} else {
				cloud.unmanagedNodes = utilsets.NewString()
				cloud.routeCIDRs = nil
			}
			if tt.nodeInformerNotSynced {
				cloud.nodeInformerSynced = func() bool { return false }
			} else {
				cloud.nodeInformerSynced = func() bool { return true }
			}

			mockVMSet.EXPECT().GetIPByNodeName(gomock.Any(), gomock.Any()).Return(nodePrivateIP, "", tt.getIPError).MaxTimes(1)
			mockVMSet.EXPECT().GetPrivateIPsByNodeName(gomock.Any(), "node").Return([]string{nodePrivateIP, "10.10.10.10"}, nil).MaxTimes(1)
			if tt.firstGetNotFound {
				mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(nil, nil).MaxTimes(1)
			} else {
				mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(initialTable, tt.getErr).MaxTimes(1)
			}
			mockRTRepo.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, routeTable armnetwork.RouteTable) (*armnetwork.RouteTable, error) {
					assert.Equal(t, updatedTable, routeTable)
					return nil, tt.createOrUpdateErr
				},
			).MaxTimes(1)

			//Here is the second invocation when route table doesn't exist
			mockRTRepo.EXPECT().Get(gomock.Any(), cloud.RouteTableName, gomock.Any()).Return(initialTable, tt.secondGetErr).MaxTimes(1)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := cloud.CreateRoute(ctx, "cluster", "unused", &route)
			assert.Equal(t, cloud.routeCIDRs, tt.expectedRouteCIDRs, tt.name)
			if err != nil {
				assert.EqualError(t, tt.expectedErrMsg, err.Error(), tt.name)
			}
		})
	}
}

func TestCreateRouteTable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRTRepo := routetable.NewMockRepository(ctrl)
	cloud := &Cloud{
		routeTableRepo: mockRTRepo,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
	}

	expectedTable := armnetwork.RouteTable{
		Name:       &cloud.RouteTableName,
		Location:   &cloud.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{},
	}
	mockRTRepo.EXPECT().CreateOrUpdate(gomock.Any(), expectedTable).Return(nil, nil)
	err := cloud.createRouteTable(context.Background())
	if err != nil {
		t.Errorf("unexpected error in creating route table: %v", err)
		t.FailNow()
	}
}

func TestProcessRoutes(t *testing.T) {
	tests := []struct {
		rt            *armnetwork.RouteTable
		expectedRoute []cloudprovider.Route
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
			name: "doesn't exist",
		},
		{
			rt:   &armnetwork.RouteTable{},
			name: "nil routes",
		},
		{
			rt: &armnetwork.RouteTable{
				Properties: &armnetwork.RouteTablePropertiesFormat{},
			},
			name: "no routes",
		},
		{
			rt: &armnetwork.RouteTable{
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: []*armnetwork.Route{
						{
							Name: ptr.To("name"),
							Properties: &armnetwork.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/16"),
							},
						},
					},
				},
			},
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
			rt: &armnetwork.RouteTable{
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: []*armnetwork.Route{
						{
							Name: ptr.To("name"),
							Properties: &armnetwork.RoutePropertiesFormat{
								AddressPrefix: ptr.To("1.2.3.4/16"),
							},
						},
						{
							Name: ptr.To("name2"),
							Properties: &armnetwork.RoutePropertiesFormat{
								AddressPrefix: ptr.To("5.6.7.8/16"),
							},
						},
					},
				},
			},
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
		routes, err := processRoutes(false, test.rt, test.err)
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
	mockVMSet := NewMockVMSet(ctrl)

	mockRTRepo := routetable.NewMockRepository(ctrl)
	cloud := &Cloud{
		routeTableRepo: mockRTRepo,
		VMSet:          mockVMSet,
		Config: Config{
			RouteTableResourceGroup: "foo",
			RouteTableName:          "bar",
			Location:                "location",
		},
		unmanagedNodes:     utilsets.NewString(),
		nodeInformerSynced: func() bool { return true },
	}
	cloud.routeUpdater = newDelayedRouteUpdater(cloud, 100*time.Millisecond)
	go cloud.routeUpdater.run(context.Background())

	testCases := []struct {
		name                  string
		routeTableName        string
		routeTable            *armnetwork.RouteTable
		hasUnmanagedNodes     bool
		nodeInformerNotSynced bool
		unmanagedNodeName     string
		routeCIDRs            map[string]string
		expectedRoutes        []*cloudprovider.Route
		getErr                error
		expectedErrMsg        error
	}{
		{
			name:           "ListRoutes should return correct routes",
			routeTableName: "rt1",
			routeTable: &armnetwork.RouteTable{
				Name:     ptr.To("rt1"),
				Location: &cloud.Location,
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: []*armnetwork.Route{
						{
							Name: ptr.To("node"),
							Properties: &armnetwork.RoutePropertiesFormat{
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
			routeTable: &armnetwork.RouteTable{
				Name:     ptr.To("rt2"),
				Location: &cloud.Location,
				Properties: &armnetwork.RouteTablePropertiesFormat{
					Routes: []*armnetwork.Route{
						{
							Name: ptr.To("node"),
							Properties: &armnetwork.RoutePropertiesFormat{
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
			routeTable:     &armnetwork.RouteTable{},
			getErr:         errors.New("dummy"),
			expectedRoutes: []*cloudprovider.Route{},
		},
		{
			name:           "ListRoutes should report error if error occurs when invoke routeTableClient.Get",
			routeTableName: "rt4",
			routeTable:     &armnetwork.RouteTable{},
			getErr:         errors.New("dummy"),
			expectedErrMsg: errors.New("dummy"),
		},
		{
			name:                  "ListRoutes should report error if node informer is not synced",
			routeTableName:        "rt5",
			nodeInformerNotSynced: true,
			routeTable:            &armnetwork.RouteTable{},
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
		mockRTRepo.EXPECT().Get(gomock.Any(), test.routeTableName, gomock.Any()).Return(test.routeTable, test.getErr)

		routes, err := cloud.ListRoutes(context.TODO(), "cluster")
		if len(test.expectedRoutes) == 0 {
			assert.Empty(t, routes, test.name)
		} else {
			assert.Equal(t, test.expectedRoutes, routes, test.name)
		}
		if test.expectedErrMsg != nil {
			assert.Error(t, err)
			assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
		}
	}
}

func TestCleanupOutdatedRoutes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, testCase := range []struct {
		description                          string
		existingRoutes, expectedRoutes       []*armnetwork.Route
		existingNodeNames                    *utilsets.IgnoreCaseSet
		expectedChanged, enableIPV6DualStack bool
	}{
		{
			description: "cleanupOutdatedRoutes should delete outdated non-dualstack routes when dualstack is enabled",
			existingRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
			},
			existingNodeNames:   utilsets.NewString("aks-node1-vmss000000"),
			enableIPV6DualStack: true,
			expectedChanged:     true,
		},
		{
			description: "cleanupOutdatedRoutes should delete outdated dualstack routes when dualstack is disabled",
			existingRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			existingNodeNames: utilsets.NewString("aks-node1-vmss000000"),
			expectedChanged:   true,
		},
		{
			description: "cleanupOutdatedRoutes should not delete unmanaged routes when dualstack is enabled",
			existingRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			existingNodeNames:   utilsets.NewString("aks-node1-vmss000001"),
			enableIPV6DualStack: true,
		},
		{
			description: "cleanupOutdatedRoutes should not delete unmanaged routes when dualstack is disabled",
			existingRoutes: []*armnetwork.Route{
				{Name: ptr.To("aks-node1-vmss000000____xxx")},
				{Name: ptr.To("aks-node1-vmss000000")},
			},
			expectedRoutes: []*armnetwork.Route{
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
	rt := &armnetwork.RouteTable{}
	tags, changed := cloud.ensureRouteTableTagged(rt)
	assert.Equal(t, expectedTags, tags)
	assert.True(t, changed)

	cloud.RouteTableResourceGroup = "rg1"
	rt = &armnetwork.RouteTable{}
	tags, changed = cloud.ensureRouteTableTagged(rt)
	assert.Nil(t, tags)
	assert.False(t, changed)
}
