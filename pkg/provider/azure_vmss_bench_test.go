/*
Copyright 2022 The Kubernetes Authors.

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
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
)

// go test -benchmem -run=^$ -count=5 -bench '^BenchmarkUpdateVMSSVMs$' sigs.k8s.io/cloud-provider-azure/pkg/provider -timeout=0
// goos: linux
// goarch: amd64
// pkg: sigs.k8s.io/cloud-provider-azure/pkg/provider
// cpu: 11th Gen Intel(R) Core(TM) i7-1185G7 @ 3.00GHz
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_1-5                      1        1004777082 ns/op           23776 B/op        230 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_1-5                      1        1008032167 ns/op           14120 B/op        192 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_1-5                      1        1009558083 ns/op           14104 B/op        191 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_1-5                      1        1013932469 ns/op           12856 B/op        184 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_1-5                      1        1013594680 ns/op           13560 B/op        189 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_2-5                      2         621996992 ns/op           15524 B/op        206 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_2-5                      2         603486975 ns/op           14124 B/op        198 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_2-5                      2         602413756 ns/op           15636 B/op        206 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_2-5                      2         612644862 ns/op           16244 B/op        207 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_2-5                      2         604255015 ns/op           14220 B/op        199 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_5-5                      5         201733780 ns/op           14587 B/op        200 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_5-5                      5         203302041 ns/op           14299 B/op        200 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_5-5                      5         202940754 ns/op           14408 B/op        199 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_5-5                      5         204206352 ns/op           13902 B/op        197 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_5-5                      5         206062474 ns/op           14206 B/op        199 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_10-5                     5         201421461 ns/op           13908 B/op        197 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_10-5                     5         201394075 ns/op           13995 B/op        197 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_10-5                     5         202607347 ns/op           15156 B/op        200 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_10-5                     5         203024935 ns/op           14529 B/op        198 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_5_batch_size_10-5                     5         201167823 ns/op           13905 B/op        197 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_1-5                     1        4045789026 ns/op           46824 B/op        693 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_1-5                     1        4057585219 ns/op           47032 B/op        695 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_1-5                     1        4063713311 ns/op           46824 B/op        693 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_1-5                     1        4037911161 ns/op           47640 B/op        699 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_1-5                     1        4061475309 ns/op           47240 B/op        696 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_2-5                     1        2027122993 ns/op           55976 B/op        768 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_2-5                     1        2026314668 ns/op           55208 B/op        766 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_2-5                     1        2010009442 ns/op           54840 B/op        761 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_2-5                     1        2016806787 ns/op           55736 B/op        768 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_2-5                     1        2028124950 ns/op           54008 B/op        757 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_5-5                     2         806128331 ns/op           53340 B/op        754 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_5-5                     2         804287660 ns/op           53236 B/op        753 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_5-5                     2         808459508 ns/op           53236 B/op        753 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_5-5                     2         804472248 ns/op           54300 B/op        761 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_5-5                     2         809618154 ns/op           53236 B/op        753 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_10-5                    3         409055472 ns/op           55176 B/op        756 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_10-5                    3         410879465 ns/op           53538 B/op        757 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_10-5                    3         417658955 ns/op           53485 B/op        754 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_10-5                    3         407526162 ns/op           53298 B/op        754 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_20_batch_size_10-5                    3         408291112 ns/op           53208 B/op        753 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_1-5                    1        20347847465 ns/op         228872 B/op       3413 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_1-5                    1        20278994738 ns/op         229688 B/op       3419 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_1-5                    1        20333078305 ns/op         230072 B/op       3422 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_1-5                    1        20335095823 ns/op         228888 B/op       3413 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_1-5                    1        20265588975 ns/op         228888 B/op       3413 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_2-5                    1        10122421534 ns/op         263320 B/op       3720 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_2-5                    1        10178407998 ns/op         263736 B/op       3723 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_2-5                    1        10199717421 ns/op         262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_2-5                    1        10166139835 ns/op         262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_2-5                    1        10114398628 ns/op         262920 B/op       3717 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_5-5                    1        4083103843 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_5-5                    1        4056605556 ns/op          262504 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_5-5                    1        4068758430 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_5-5                    1        4029400400 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_5-5                    1        4073177810 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_10-5                   1        2012005065 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_10-5                   1        2011972323 ns/op          262776 B/op       3715 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_10-5                   1        2041940768 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_10-5                   1        2014742464 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_100_batch_size_10-5                   1        2017203321 ns/op          262520 B/op       3714 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_1-5                   1        101900562893 ns/op       1137672 B/op      17013 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_1-5                    1        101412493739 ns/op       1137672 B/op      17013 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_1-5                    1        101685370283 ns/op       1138072 B/op      17016 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_1-5                    1        101514837421 ns/op       1138488 B/op      17019 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_1-5                    1        101622046419 ns/op       1137672 B/op      17013 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_2-5                    1        51031012310 ns/op        1306920 B/op      18523 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_2-5                    1        50895108034 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_2-5                    1        50797118749 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_2-5                    1        50773342073 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_2-5                    1        50605629098 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_5-5                    1        20299650018 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_5-5                    1        20107559039 ns/op        1305704 B/op      18512 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_5-5                    1        20354770109 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_5-5                    1        20316420918 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_5-5                    1        20082889384 ns/op        1305720 B/op      18514 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_10-5                   1        10050288652 ns/op        1313016 B/op      18535 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_10-5                   1        10053444017 ns/op        1307032 B/op      18526 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_10-5                   1        10050662117 ns/op        1306392 B/op      18520 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_10-5                   1        10056109620 ns/op        1306200 B/op      18519 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_500_batch_size_10-5                   1        10052595290 ns/op        1306488 B/op      18522 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_1-5                   1        202184504123 ns/op       2274688 B/op      34015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_1-5                   1        202621583245 ns/op       2274704 B/op      34015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_1-5                   1        203072169346 ns/op       2274704 B/op      34015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_1-5                   1        203493224379 ns/op       2274688 B/op      34015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_1-5                   1        203044039412 ns/op       2274704 B/op      34015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_2-5                   1        101410877061 ns/op       2611648 B/op      37023 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_2-5                   1        101730978024 ns/op       2610848 B/op      37017 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_2-5                   1        101555260048 ns/op       2611728 B/op      37025 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_2-5                   1        101659685526 ns/op       2612080 B/op      37019 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_2-5                   1        101649890958 ns/op       2611024 B/op      37019 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_5-5                   1        40679144168 ns/op        2611776 B/op      37027 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_5-5                   1        40826075485 ns/op        2611632 B/op      37015 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_5-5                   1        40706661152 ns/op        2610928 B/op      37018 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_5-5                   1        40719464773 ns/op        2615440 B/op      37032 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_5-5                   1        40683037070 ns/op        2610752 B/op      37014 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_10-5                  1        20396002069 ns/op        2611520 B/op      37022 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_10-5                  1        20284312819 ns/op        2612368 B/op      37033 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_10-5                  1        20335551936 ns/op        2612592 B/op      37030 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_10-5                  1        20344432668 ns/op        2614112 B/op      37051 allocs/op
// BenchmarkUpdateVMSSVMs/vmss_size_1000_batch_size_10-5                  1        20292074046 ns/op        2613632 B/op      37024 allocs/op
// PASS
// ok      sigs.k8s.io/cloud-provider-azure/pkg/provider   3013.951s
func BenchmarkUpdateVMSSVMs(b *testing.B) {
	ctrl := gomock.NewController(b)
	defer ctrl.Finish()
	vmssvmClient := mock_virtualmachinescalesetvmclient.NewMockInterface(ctrl)
	vmssvmClient.EXPECT().BeginUpdate(gomock.Any(), "rg", "vmss", gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, _, _ string, _ armcompute.VirtualMachineScaleSetVM, _ *armcompute.VirtualMachineScaleSetVMsClientBeginUpdateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientUpdateResponse], error) {
		time.Sleep(200 * time.Millisecond)
		poller, _ := runtime.NewPoller[armcompute.VirtualMachineScaleSetVMsClientUpdateResponse](
			&http.Response{
				Header: http.Header{"Fake-Poller-Status": []string{"https://fake/operation"}},
			},
			runtime.Pipeline{},
			&runtime.NewPollerOptions[armcompute.VirtualMachineScaleSetVMsClientUpdateResponse]{
				Handler: &vmssvmMockPutHandler{
					sleep: 559669,
					resp: &http.Response{
						StatusCode: http.StatusAccepted,
					},
				},
			},
		)
		return poller, nil
	}).AnyTimes()
	clientFactory := mock_azclient.NewMockClientFactory(ctrl)
	clientFactory.EXPECT().GetVirtualMachineScaleSetVMClient().Return(vmssvmClient).AnyTimes()
	scaleset := ScaleSet{
		Cloud: &Cloud{ComputeClientFactory: clientFactory},
	}
	for _, vmsssize := range []int{5, 20, 100, 500, 1000} {
		for _, batchSize := range []int{1, 2, 5, 10} {
			b.Run("vmss size "+strconv.Itoa(vmsssize)+" batch size "+strconv.Itoa(batchSize), func(b *testing.B) {
				vmssMetaInfo := vmssMetaInfo{
					resourceGroup: "rg",
					vmssName:      "vmss",
				}
				vmUpdates := genTestvms(vmsssize)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StartTimer()
					errChan := scaleset.UpdateVMSSVMsInBatch(context.Background(), vmssMetaInfo, vmUpdates, batchSize)
					b.StopTimer()
					for err := range errChan {
						if err != nil {
							b.Error(err)
						}
					}
				}
			})
		}
	}
}

func genTestvms(size int) map[string]armcompute.VirtualMachineScaleSetVM {
	vms := make(map[string]armcompute.VirtualMachineScaleSetVM)
	for i := 0; i < size; i++ {
		vms[strconv.Itoa(i)] = armcompute.VirtualMachineScaleSetVM{
			InstanceID: to.Ptr(strconv.Itoa(i)),
		}
	}
	return vms
}
