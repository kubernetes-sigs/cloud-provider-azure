package vmssvmclient

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
)

// VirtualMachineScaleSetVM wraps the original VirtualMachineScaleSetVM struct and adds an Etag field.
type VirtualMachineScaleSetVM struct {
	compute.VirtualMachineScaleSetVM `json:",inline"`
	// READ-ONLY; Etag is property returned in Update/Get response of the VMSS VM, so that customer can supply it in the header
	// to ensure optimistic updates.
	Etag *string `json:"etag,omitempty"`
}

// VirtualMachineScaleSetVMListResult the List Virtual Machine operation response.
type VirtualMachineScaleSetVMListResult struct {
	autorest.Response `json:"-"`
	// Value - The list of virtual machine scale sets.
	Value *[]VirtualMachineScaleSetVM `json:"value,omitempty"`
	// NextLink - The uri to fetch the next page of Virtual Machine Scale Sets. Call ListNext() with this to fetch the next page of VMSS.
	NextLink *string `json:"nextLink,omitempty"`
}

// IsEmpty returns true if the ListResult contains no values.
func (vmssvmlr VirtualMachineScaleSetVMListResult) IsEmpty() bool {
	return vmssvmlr.Value == nil || len(*vmssvmlr.Value) == 0
}

// hasNextLink returns true if the NextLink is not empty.
func (vmssvmlr VirtualMachineScaleSetVMListResult) hasNextLink() bool {
	return vmssvmlr.NextLink != nil && len(*vmssvmlr.NextLink) != 0
}

// virtualMachineScaleSetListResultPreparer prepares a request to retrieve the next set of results.
// It returns nil if no more results exist.
func (vmssvmlr VirtualMachineScaleSetVMListResult) virtualMachineScaleSetListResultPreparer(ctx context.Context) (*http.Request, error) {
	if !vmssvmlr.hasNextLink() {
		return nil, nil
	}
	return autorest.Prepare((&http.Request{}).WithContext(ctx),
		autorest.AsJSON(),
		autorest.AsGet(),
		autorest.WithBaseURL(to.String(vmssvmlr.NextLink)))
}

// UnmarshalJSON is the custom unmarshaler for VirtualMachineScaleSetVM struct.
// compute.VirtualMachineScaleSetVM implemented `UnmarshalJSON` method, and when the response is unmarshaled into VirtualMachineScaleSetVM,
// compute.VirtualMachineScaleSetVM.UnmarshalJSON is called, leading to the loss of the Etag field.
func (vmssvm *VirtualMachineScaleSetVM) UnmarshalJSON(data []byte) error {
	// Unmarshal Etag first
	etagPlaceholder := struct {
		Etag *string `json:"etag,omitempty"`
	}{}
	if err := json.Unmarshal(data, &etagPlaceholder); err != nil {
		return err
	}
	// Unmarshal Nested VirtualMachineScaleSetVM
	nestedVirtualMachineScaleSetVM := struct {
		compute.VirtualMachineScaleSetVM `json:",inline"`
	}{}
	// the Nested impl UnmarshalJSON, so it should be unmarshaled alone
	if err := json.Unmarshal(data, &nestedVirtualMachineScaleSetVM); err != nil {
		return err
	}
	(vmssvm).Etag = etagPlaceholder.Etag
	(vmssvm).VirtualMachineScaleSetVM = nestedVirtualMachineScaleSetVM.VirtualMachineScaleSetVM
	return nil
}

// MarshalJSON is the custom marshaler for VirtualMachineScaleSetVM.
func (vmssv VirtualMachineScaleSetVM) MarshalJSON() ([]byte, error) {
	var err error
	var nestedVirtualMachineScaleSetVMJSON, etagJSON []byte
	if nestedVirtualMachineScaleSetVMJSON, err = vmssv.VirtualMachineScaleSetVM.MarshalJSON(); err != nil {
		return nil, err
	}

	if vmssv.Etag != nil {
		if etagJSON, err = json.Marshal(map[string]interface{}{
			"etag": vmssv.Etag,
		}); err != nil {
			return nil, err
		}
	}

	// empty struct can be Unmarshaled to "{}"
	nestedVirtualMachineScaleSetVMJSONEmpty := true
	if string(nestedVirtualMachineScaleSetVMJSON) != "{}" {
		nestedVirtualMachineScaleSetVMJSONEmpty = false
	}
	etagJSONEmpty := true
	if len(etagJSON) != 0 {
		etagJSONEmpty = false
	}

	// when both parts not empty, join the two parts with a comma but remove the open brace of nestedVirtualMachineScaleSetVMJson and the close brace of the etagJSON
	// {"location": "eastus"} + {"etag": "\"120\""} will be merged into {"location": "eastus", "etag": "\"120\""}
	if !nestedVirtualMachineScaleSetVMJSONEmpty && !etagJSONEmpty {
		etagJSON[0] = ','
		return append(nestedVirtualMachineScaleSetVMJSON[:len(nestedVirtualMachineScaleSetVMJSON)-1], etagJSON...), nil
	}
	if !nestedVirtualMachineScaleSetVMJSONEmpty {
		return nestedVirtualMachineScaleSetVMJSON, nil
	}
	if !etagJSONEmpty {
		return etagJSON, nil
	}
	return []byte("{}"), nil
}
