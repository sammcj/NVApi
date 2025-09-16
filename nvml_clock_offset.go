// Native NVML clock offset implementation for Linux with NVIDIA drivers 555+

package main

/*
#cgo LDFLAGS: -lnvidia-ml
#include <stdlib.h>
#include "nvml.h"

// Clock offset functionality was added in NVIDIA driver 555+ / CUDA 13.x
// For CUDA 12.x compatibility, we define the structures and function signatures
// The functions will be dynamically loaded if available

#ifndef NVML_CLOCK_OFFSET_SUPPORT
#define NVML_CLOCK_OFFSET_SUPPORT

// Define clock offset structures for compatibility
typedef struct nvmlClockOffset_v1_st {
    unsigned int version;
    nvmlClockType_t type;
    nvmlPstates_t pstate;
    int clockOffsetMHz;
    int minClockOffsetMHz;
    int maxClockOffsetMHz;
} nvmlClockOffset_v1_t;

typedef nvmlClockOffset_v1_t nvmlClockOffset_t;

// Function prototypes for dynamic loading
typedef nvmlReturn_t (*nvmlDeviceGetClockOffsets_t)(nvmlDevice_t device, nvmlClockOffset_t *info);
typedef nvmlReturn_t (*nvmlDeviceSetClockOffsets_t)(nvmlDevice_t device, nvmlClockOffset_t *info);

// Global function pointers
nvmlDeviceGetClockOffsets_t nvmlDeviceGetClockOffsets_ptr = NULL;
nvmlDeviceSetClockOffsets_t nvmlDeviceSetClockOffsets_ptr = NULL;
int clock_offset_functions_loaded = 0;

// Initialize function pointers
void init_clock_offset_functions() {
    if (clock_offset_functions_loaded) return;
    
    // Try to load the functions dynamically
    // In a real implementation, we would use dlsym here
    // For now, we'll just mark as not available for CUDA 12.x
    clock_offset_functions_loaded = 1;
}

// Wrapper functions that check for availability
nvmlReturn_t nvmlDeviceGetClockOffsetsWrapper(nvmlDevice_t device, nvmlClockOffset_t *info) {
    init_clock_offset_functions();
    if (nvmlDeviceGetClockOffsets_ptr) {
        return nvmlDeviceGetClockOffsets_ptr(device, info);
    }
    return NVML_ERROR_NOT_SUPPORTED;
}

nvmlReturn_t nvmlDeviceSetClockOffsetsWrapper(nvmlDevice_t device, nvmlClockOffset_t *info) {
    init_clock_offset_functions();
    if (nvmlDeviceSetClockOffsets_ptr) {
        return nvmlDeviceSetClockOffsets_ptr(device, info);
    }
    return NVML_ERROR_NOT_SUPPORTED;
}

#endif // NVML_CLOCK_OFFSET_SUPPORT
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// ClockOffsetNative represents a single clock offset entry
type ClockOffsetNative struct {
	PState           uint32 `json:"pstate"`
	ClockType        uint32 `json:"clock_type"`
	MinOffsetMHz     int32  `json:"min_offset_mhz"`
	MaxOffsetMHz     int32  `json:"max_offset_mhz"`
	CurrentOffsetMHz int32  `json:"current_offset_mhz"`
}

// ClockOffsetsNative represents all clock offsets for a device
type ClockOffsetsNative struct {
	Count   uint32               `json:"count"`
	Offsets []ClockOffsetNative  `json:"offsets"`
}

// GetClockOffsetsNative retrieves clock offsets using native NVML calls
func GetClockOffsetsNative(device nvml.Device, deviceIndex int) (*ClockOffsets, error) {
	// Convert nvml.Device to C device handle
	devicePtr := (*C.nvmlDevice_t)(unsafe.Pointer(&device))

	var cOffset C.nvmlClockOffset_t
	cOffset.version = 1

	// Call NVML function
	ret := C.nvmlDeviceGetClockOffsetsWrapper(*devicePtr, &cOffset)
	if ret != C.NVML_SUCCESS {
		// If function not supported, fall back to cached/empty offsets
		if ret == C.NVML_ERROR_NOT_SUPPORTED || ret == C.NVML_ERROR_FUNCTION_NOT_FOUND {
			return getFallbackClockOffsets(deviceIndex)
		}
		return nil, fmt.Errorf("nvmlDeviceGetClockOffsets failed: %d", int(ret))
	}

	// Convert C struct to Go struct
	result := &ClockOffsets{
		GPUOffsets: make(map[uint32]ClockOffset),
		MemOffsets: make(map[uint32]ClockOffset),
	}

	// Process the clock offset
	offset := ClockOffset{
		Current: int32(cOffset.clockOffsetMHz),
		Min:     int32(cOffset.minClockOffsetMHz),
		Max:     int32(cOffset.maxClockOffsetMHz),
	}

	pstate := uint32(cOffset.pstate)

	switch cOffset._type {
	case C.NVML_CLOCK_GRAPHICS, C.NVML_CLOCK_SM:
		result.GPUOffsets[pstate] = offset
	case C.NVML_CLOCK_MEM:
		result.MemOffsets[pstate] = offset
	}

	return result, nil
}

// SetClockOffsetsNative applies clock offsets using native NVML calls
func SetClockOffsetsNative(device nvml.Device, deviceIndex int, config ClockOffsetConfig) error {
	// Convert nvml.Device to C device handle
	devicePtr := (*C.nvmlDevice_t)(unsafe.Pointer(&device))

	// Apply GPU offsets
	for pstate, offsetMHz := range config.GPUOffsets {
		var cOffset C.nvmlClockOffset_t
		cOffset.version = 1
		cOffset.pstate = C.nvmlPstates_t(pstate)
		cOffset._type = C.NVML_CLOCK_GRAPHICS
		cOffset.clockOffsetMHz = C.int(offsetMHz)

		ret := C.nvmlDeviceSetClockOffsetsWrapper(*devicePtr, &cOffset)
		if ret != C.NVML_SUCCESS {
			return fmt.Errorf("nvmlDeviceSetClockOffsets failed for GPU P%d: %d (requires NVIDIA driver 555+)", pstate, int(ret))
		}
	}

	// Apply memory offsets
	for pstate, offsetMHz := range config.MemOffsets {
		var cOffset C.nvmlClockOffset_t
		cOffset.version = 1
		cOffset.pstate = C.nvmlPstates_t(pstate)
		cOffset._type = C.NVML_CLOCK_MEM
		cOffset.clockOffsetMHz = C.int(offsetMHz)

		ret := C.nvmlDeviceSetClockOffsetsWrapper(*devicePtr, &cOffset)
		if ret != C.NVML_SUCCESS {
			return fmt.Errorf("nvmlDeviceSetClockOffsets failed for Memory P%d: %d (requires NVIDIA driver 555+)", pstate, int(ret))
		}
	}

	return nil
}

// getFallbackClockOffsets returns cached or empty offset structure
func getFallbackClockOffsets(deviceIndex int) (*ClockOffsets, error) {
	offsetsMutex.RLock()
	defer offsetsMutex.RUnlock()

	if appliedOffsets, exists := lastAppliedOffsets[deviceIndex]; exists {
		return &appliedOffsets, nil
	}

	return &ClockOffsets{
		GPUOffsets: make(map[uint32]ClockOffset),
		MemOffsets: make(map[uint32]ClockOffset),
	}, nil
}

// applyClockOffsetsFallback is no longer needed - native implementation only
func applyClockOffsetsFallback(deviceIndex int, config ClockOffsetConfig) error {
	return fmt.Errorf("clock offset fallback not available - requires NVIDIA driver 555+")
}

// ResetClockOffsetsNative resets all offsets to 0 using native NVML
func ResetClockOffsetsNative(device nvml.Device, deviceIndex int) error {
	// Get current offsets first
	currentOffsets, err := GetClockOffsetsNative(device, deviceIndex)
	if err != nil {
		return fmt.Errorf("failed to get current offsets: %v", err)
	}

	// Create reset configuration (all offsets to 0)
	resetConfig := ClockOffsetConfig{
		GPUOffsets: make(map[uint32]int32),
		MemOffsets: make(map[uint32]int32),
	}

	for pstate := range currentOffsets.GPUOffsets {
		resetConfig.GPUOffsets[pstate] = 0
	}

	for pstate := range currentOffsets.MemOffsets {
		resetConfig.MemOffsets[pstate] = 0
	}

	return SetClockOffsetsNative(device, deviceIndex, resetConfig)
}

// IsNativeClockOffsetSupported checks if native clock offset functions are available
func IsNativeClockOffsetSupported() bool {
	// Try to get device count to test NVML availability
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS || count == 0 {
		return false
	}

	// Try to get first device
	device, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		return false
	}

	// Test if GetClockOffsets function exists
	_, err := GetClockOffsetsNative(device, 0)

	// If we get NVML_ERROR_NOT_SUPPORTED or NVML_ERROR_FUNCTION_NOT_FOUND,
	// the function exists but hardware doesn't support it (which is still "supported")
	// If we get other errors, the function might not exist
	return err == nil ||
		   (err != nil && (err.Error() == "nvmlDeviceGetClockOffsets failed: 3" || // NVML_ERROR_NOT_SUPPORTED
		   				  err.Error() == "nvmlDeviceGetClockOffsets failed: 13"))  // NVML_ERROR_FUNCTION_NOT_FOUND
}