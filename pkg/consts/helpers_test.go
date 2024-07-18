/*
Copyright 2021 The Kubernetes Authors.

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

// Package consts stages all the consts under pkg/.
package consts

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestIsK8sServiceHasHAModeEnabled(t *testing.T) {
	type args struct {
		service *v1.Service
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "ha mode is enabled",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
						},
					},
				},
			},
			want: true,
		},
		{
			name: "ha mode is missing",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{},
					},
				},
			},
			want: false,
		},
		{
			name: "ha mode is corrupted",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true1",
						},
					},
				},
			},
			want: false,
		},
		{
			name: "ha annotation key is missing",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts + "1": "true1",
						},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsK8sServiceHasHAModeEnabled(tt.args.service); got != tt.want {
				t.Errorf("IsK8sServiceHasHAModeEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsK8sServiceUsingInternalLoadBalancer(t *testing.T) {
	type args struct {
		service *v1.Service
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "internal flag is set",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{ServiceAnnotationLoadBalancerInternal: TrueAnnotationValue},
					},
				},
			},
			want: true,
		},
		{
			name: "internal flag is not set",
			args: args{
				service: &v1.Service{},
			},
			want: false,
		},
		{
			name: "internal flag is corrupted",
			args: args{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{ServiceAnnotationLoadBalancerInternal: TrueAnnotationValue + TrueAnnotationValue},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsK8sServiceUsingInternalLoadBalancer(tt.args.service); got != tt.want {
				t.Errorf("IsK8sServiceUsingInternalLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetHealthProbeConfigOfPortFromK8sSvcAnnotation(t *testing.T) {
	type args struct {
		annotations map[string]string
		port        int32
		key         HealthProbeParams
		validators  []BusinessValidator
	}
	tests := []struct {
		name    string
		args    args
		want    *string
		wantErr bool
	}{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetHealthProbeConfigOfPortFromK8sSvcAnnotation(tt.args.annotations, tt.args.port, tt.args.key, tt.args.validators...)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetHealthProbeConfigOfPortFromK8sSvcAnnotation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetHealthProbeConfigOfPortFromK8sSvcAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_extractInt32FromString(t *testing.T) {
	type args struct {
		val               string
		businessValidator []Int32BusinessValidator
	}
	tests := []struct {
		name    string
		args    args
		want    *int32
		wantErr bool
	}{
		{name: "value is not a number", args: args{val: "cookies"}, wantErr: true},
		{name: "value is zero", args: args{val: "0"}, wantErr: false, want: ptr.To(int32(0))},
		{name: "value is a float number", args: args{val: "0.1"}, wantErr: true},
		{name: "value is a positive integer", args: args{val: "24"}, want: ptr.To(int32(24)), wantErr: false},
		{name: "value negative integer", args: args{val: "-6"}, want: ptr.To(int32(-6)), wantErr: false},
		{name: "validator is nil", args: args{val: "-6", businessValidator: []Int32BusinessValidator{
			nil,
		}}, want: ptr.To(int32(-6)), wantErr: false},
		{name: "validation failed", args: args{val: "-6", businessValidator: []Int32BusinessValidator{
			func(i *int32) error {
				return fmt.Errorf("validator failed")
			},
		}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractInt32FromString(tt.args.val, tt.args.businessValidator...)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractInt32FromString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractInt32FromString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_getAttributeValueInSvcAnnotation(t *testing.T) {
	type args struct {
		annotations map[string]string
		key         string
		validators  []BusinessValidator
	}
	tests := []struct {
		name    string
		args    args
		want    *string
		wantErr bool
	}{
		{name: "annotation set is empty", args: args{key: "key"}, want: nil, wantErr: false},
		{name: "key is not specified even though annotation set is not empty", args: args{annotations: map[string]string{"key": ""}}, want: nil, wantErr: false},
		{name: "validation failed", args: args{annotations: map[string]string{"key": ""}, key: "key", validators: []BusinessValidator{
			func(s *string) error {
				return fmt.Errorf("validator failed")
			},
		}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetAttributeValueInSvcAnnotation(tt.args.annotations, tt.args.key, tt.args.validators...)
			if (err != nil) != tt.wantErr {
				t.Errorf("getAttributeValueInSvcAnnotation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getAttributeValueInSvcAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_expectAttributeInSvcAnnotationBeEqualTo(t *testing.T) {
	type args struct {
		annotations map[string]string
		key         string
		value       string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "assertion is successful",
			args: args{
				annotations: map[string]string{
					ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
				},
				key:   ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts,
				value: "true",
			},
			want: true,
		}, {
			name: "assertion is successful especially when value is not exactly equal",
			args: args{
				annotations: map[string]string{
					ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "True",
				},
				key:   ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts,
				value: "true",
			},
			want: true,
		},
		{
			name: "assertion is unsuccessful when key is not equal",
			args: args{
				annotations: map[string]string{
					ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "True",
				},
				key:   strings.ToUpper(ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts),
				value: "true",
			},
			want: false,
		},
		{
			name: "assertion is unsuccessful when key is not found",
			args: args{
				annotations: map[string]string{
					ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "True",
				},
				key:   ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts + "notfound",
				value: "true",
			},
			want: false,
		},
		{
			name: "assertion is unsuccessful when value is empty",
			args: args{
				annotations: map[string]string{
					ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "",
				},
				key:   ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts,
				value: "true",
			},
			want: false,
		},
		{
			name: "assertion is unsuccessful when value is notfound",
			args: args{
				annotations: map[string]string{},
				key:         ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts,
				value:       "true",
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expectAttributeInSvcAnnotationBeEqualTo(tt.args.annotations, tt.args.key, tt.args.value); got != tt.want {
				t.Errorf("expectAttributeInSvcAnnotationBeEqualTo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetInt32HealthProbeConfigOfPortFromK8sSvcAnnotation(t *testing.T) {
	type args struct {
		annotations map[string]string
		port        int32
		key         HealthProbeParams
		validators  []Int32BusinessValidator
	}
	tests := []struct {
		name    string
		args    args
		want    *int32
		wantErr bool
	}{
		{
			name: "get numeric value from health probe related annotation",
			args: args{
				annotations: map[string]string{BuildHealthProbeAnnotationKeyForPort(80, HealthProbeParamsNumOfProbe): "2"},
				port:        80,
				key:         HealthProbeParamsNumOfProbe,
			},
			want:    ptr.To(int32(2)),
			wantErr: false,
		},
		{
			name: "health probe related annotation is not found",
			args: args{
				annotations: map[string]string{},
				port:        80,
				key:         HealthProbeParamsNumOfProbe,
			},
			want:    nil,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetInt32HealthProbeConfigOfPortFromK8sSvcAnnotation(tt.args.annotations, tt.args.port, tt.args.key, tt.args.validators...)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetInt32HealthProbeConfigOfPortFromK8sSvcAnnotation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetInt32HealthProbeConfigOfPortFromK8sSvcAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildAnnotationKeyForPort(t *testing.T) {
	type args struct {
		port int32
		key  PortParams
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "no lb rule",
			args: args{
				port: 80,
				key:  PortAnnotationNoLBRule,
			},
			want: "service.beta.kubernetes.io/port_80_no_lb_rule",
		},
		{
			name: "no lb rule",
			args: args{
				port: 80,
				key:  PortAnnotationNoHealthProbeRule,
			},
			want: "service.beta.kubernetes.io/port_80_no_probe_rule",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildAnnotationKeyForPort(tt.args.port, tt.args.key); got != tt.want {
				t.Errorf("BuildAnnotationKeyForPort() = %v, want %v", got, tt.want)
			}
		})
	}
}
