//go:build linux
// +build linux

// Native NVML clock offset implementation for Linux with NVIDIA drivers 555+

package main

/*
#cgo LDFLAGS: -lnvidia-ml -ldl
#include <stdlib.h>
#include <stdio.h>
#include <dlfcn.h>
#include "nvml.h"

// Clock offset functionality was added in NVIDIA driver 555+ / CUDA 13.x
// For CUDA 12.x compatibility, we define the structures and function signatures
// The functions will be dynamically loaded if available

#ifndef NVML_CLOCK_OFFSET_SUPPORT
#define NVML_CLOCK_OFFSET_SUPPORT

// Define clock offset structures for compatibility
typedef struct nvmlClockOffset_v1_st {
    unsigned int version;
    nvmlClockType_t clockType;
    nvmlPstates_t pstate;
    int clockOffsetMHz;
    int minClockOffsetMHz;
    int maxClockOffsetMHz;
} nvmlClockOffset_v1_t;

typedef nvmlClockOffset_v1_t nvmlClockOffset_t;

// Version macro for the clock offset struct
#define NVML_CLOCK_OFFSET_VERSION_1 (unsigned int)(sizeof(nvmlClockOffset_v1_t) | (1 << 24))

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

    // Try to load the functions dynamically from libnvidia-ml.so
    void* handle = dlopen("libnvidia-ml.so.1", RTLD_LAZY);
    if (handle) {
        nvmlDeviceGetClockOffsets_ptr = (nvmlDeviceGetClockOffsets_t)dlsym(handle, "nvmlDeviceGetClockOffsets");
        nvmlDeviceSetClockOffsets_ptr = (nvmlDeviceSetClockOffsets_t)dlsym(handle, "nvmlDeviceSetClockOffsets");

        // Debug: check if functions were loaded
        printf("DEBUG: dlsym nvmlDeviceGetClockOffsets: %p\n", nvmlDeviceGetClockOffsets_ptr);
        printf("DEBUG: dlsym nvmlDeviceSetClockOffsets: %p\n", nvmlDeviceSetClockOffsets_ptr);
    } else {
        printf("DEBUG: dlopen failed: %s\n", dlerror());
    }

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

// Wrapper that gets device by index and retrieves offsets
nvmlReturn_t nvmlDeviceGetClockOffsetsByIndex(unsigned int index, nvmlClockType_t clockType, nvmlPstates_t pstate, nvmlClockOffset_t *info, int debug_mode) {
    init_clock_offset_functions();
    if (!nvmlDeviceGetClockOffsets_ptr) {
        return NVML_ERROR_NOT_SUPPORTED;
    }

    nvmlDevice_t device;
    nvmlReturn_t ret = nvmlDeviceGetHandleByIndex(index, &device);
    if (ret != NVML_SUCCESS) {
        return ret;
    }

    info->version = NVML_CLOCK_OFFSET_VERSION_1;
    info->clockType = clockType;
    info->pstate = pstate;

    return nvmlDeviceGetClockOffsets_ptr(device, info);
}

nvmlReturn_t nvmlDeviceSetClockOffsetsWrapper(nvmlDevice_t device, nvmlClockOffset_t *info) {
    init_clock_offset_functions();
    if (nvmlDeviceSetClockOffsets_ptr) {
        return nvmlDeviceSetClockOffsets_ptr(device, info);
    }
    return NVML_ERROR_NOT_SUPPORTED;
}

// Wrapper that gets device by index and sets offsets
nvmlReturn_t nvmlDeviceSetClockOffsetsByIndex(unsigned int index, nvmlClockType_t clockType, nvmlPstates_t pstate, int offset, int debug_mode) {
    init_clock_offset_functions();
    if (!nvmlDeviceSetClockOffsets_ptr) {
        return NVML_ERROR_NOT_SUPPORTED;
    }

    nvmlDevice_t device;
    nvmlReturn_t ret = nvmlDeviceGetHandleByIndex(index, &device);
    if (ret != NVML_SUCCESS) {
        return ret;
    }

    nvmlClockOffset_t info;
    info.version = NVML_CLOCK_OFFSET_VERSION_1;
    info.clockType = clockType;
    info.pstate = pstate;

    // First get the current values to preserve min/max
    if (nvmlDeviceGetClockOffsets_ptr) {
        ret = nvmlDeviceGetClockOffsets_ptr(device, &info);
        if (ret == NVML_ERROR_INVALID_ARGUMENT || ret == NVML_ERROR_NOT_SUPPORTED) {
            if (debug_mode) {
                printf("DEBUG: P-state %d not supported for clock type %d\n", pstate, clockType);
            }
            return ret;
        }
    }

    // Now set the new offset
    info.clockOffsetMHz = offset;
    return nvmlDeviceSetClockOffsets_ptr(device, &info);
}

#endif // NVML_CLOCK_OFFSET_SUPPORT
*/
import "C"
import (
	"fmt"

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
	Count   uint32              `json:"count"`
	Offsets []ClockOffsetNative `json:"offsets"`
}

// GetClockOffsetsNative retrieves clock offsets using native NVML calls
func GetClockOffsetsNative(device nvml.Device, deviceIndex int) (*ClockOffsets, error) {
	result := &ClockOffsets{
		GPUOffsets: make(map[uint32]ClockOffset),
		MemOffsets: make(map[uint32]ClockOffset),
	}

	// Try to query clock offsets for different P-states and clock types
	clockTypes := []C.nvmlClockType_t{C.NVML_CLOCK_GRAPHICS, C.NVML_CLOCK_MEM}
	pstates := []C.nvmlPstates_t{C.NVML_PSTATE_0, C.NVML_PSTATE_1, C.NVML_PSTATE_2}

	for _, clockType := range clockTypes {
		for _, pstate := range pstates {
			var cOffset C.nvmlClockOffset_t
			ret := C.nvmlDeviceGetClockOffsetsByIndex(
				C.uint(deviceIndex),
				clockType,
				pstate,
				&cOffset,
				func() C.int {
					if debug != nil && *debug {
						return 1
					}
					return 0
				}(),
			)
			if ret == C.NVML_SUCCESS {
				offset := ClockOffset{
					Current: int32(cOffset.clockOffsetMHz),
					Min:     int32(cOffset.minClockOffsetMHz),
					Max:     int32(cOffset.maxClockOffsetMHz),
				}

				pstateIdx := uint32(pstate)
				clockName := "GPU"
				if clockType == C.NVML_CLOCK_MEM {
					clockName = "MEM"
				}

				switch clockType {
				case C.NVML_CLOCK_GRAPHICS, C.NVML_CLOCK_SM:
					result.GPUOffsets[pstateIdx] = offset
				case C.NVML_CLOCK_MEM:
					result.MemOffsets[pstateIdx] = offset
				}

				if debug != nil && *debug {
					fmt.Printf("DEBUG: Successfully read %s P%d offset: %d MHz\n", clockName, pstate, cOffset.clockOffsetMHz)
				}
			} else if ret == C.NVML_ERROR_INVALID_ARGUMENT {
				if debug != nil && *debug {
					fmt.Printf("DEBUG: P-state %d not supported for clock type %d\n", pstate, clockType)
				}
			} else if ret != C.NVML_ERROR_NOT_SUPPORTED {
				if debug != nil && *debug {
					fmt.Printf("DEBUG: Query failed for clock type %d P-state %d: error %d\n", clockType, pstate, ret)
				}
			}
		}
	}

	return result, nil
}

// SetClockOffsetsNative sets clock offsets using native NVML calls
func SetClockOffsetsNative(device nvml.Device, deviceIndex int, config ClockOffsetConfig) error {
	// We'll use the device index to get the handle directly in C
	// This avoids the complexity of converting the Go Device interface

	// Apply GPU offsets
	for pstate, offset := range config.GPUOffsets {
		ret := C.nvmlDeviceSetClockOffsetsByIndex(
			C.uint(deviceIndex),
			C.NVML_CLOCK_GRAPHICS,
			C.nvmlPstates_t(pstate),
			C.int(offset),
			func() C.int {
				if debug != nil && *debug {
					return 1
				}
				return 0
			}(),
		)
		if ret == C.NVML_SUCCESS {
			if debug != nil && *debug {
				fmt.Printf("DEBUG: Successfully set GPU P%d offset to %d MHz\n", pstate, offset)
			}
		} else if ret == C.NVML_ERROR_INVALID_ARGUMENT {
			if debug != nil && *debug {
				fmt.Printf("DEBUG: GPU P-state %d not supported for setting\n", pstate)
			}
		} else if ret != C.NVML_ERROR_NOT_SUPPORTED {
			return fmt.Errorf("failed to set GPU P%d clock offset: NVML error %d", pstate, ret)
		}
	}

	// Apply memory offsets
	for pstate, offset := range config.MemOffsets {
		ret := C.nvmlDeviceSetClockOffsetsByIndex(
			C.uint(deviceIndex),
			C.NVML_CLOCK_MEM,
			C.nvmlPstates_t(pstate),
			C.int(offset),
			func() C.int {
				if debug != nil && *debug {
					return 1
				}
				return 0
			}(),
		)
		if ret == C.NVML_SUCCESS {
			if debug != nil && *debug {
				fmt.Printf("DEBUG: Successfully set Memory P%d offset to %d MHz\n", pstate, offset)
			}
		} else if ret == C.NVML_ERROR_INVALID_ARGUMENT {
			if debug != nil && *debug {
				fmt.Printf("DEBUG: Memory P-state %d not supported for setting\n", pstate)
			}
		} else if ret != C.NVML_ERROR_NOT_SUPPORTED {
			return fmt.Errorf("failed to set Memory P%d clock offset: NVML error %d", pstate, ret)
		}
	}

	return nil
}

// ResetClockOffsetsNative resets clock offsets to 0 using native NVML calls
func ResetClockOffsetsNative(device nvml.Device, deviceIndex int) error {
	// Reset GPU and memory offsets for common P-states
	clockTypes := []C.nvmlClockType_t{C.NVML_CLOCK_GRAPHICS, C.NVML_CLOCK_MEM}
	pstates := []C.nvmlPstates_t{C.NVML_PSTATE_0, C.NVML_PSTATE_1, C.NVML_PSTATE_2}

	for _, clockType := range clockTypes {
		for _, pstate := range pstates {
			// Set offset to 0 to reset
			ret := C.nvmlDeviceSetClockOffsetsByIndex(
				C.uint(deviceIndex),
				clockType,
				pstate,
				C.int(0),
				func() C.int {
					if debug != nil && *debug {
						return 1
					}
					return 0
				}(),
			)

			if ret == C.NVML_SUCCESS {
				clockName := "GPU"
				if clockType == C.NVML_CLOCK_MEM {
					clockName = "Memory"
				}
				if debug != nil && *debug {
					fmt.Printf("DEBUG: Reset %s P%d offset to 0 MHz on GPU %d\n", clockName, pstate, deviceIndex)
				}
			} else if ret == C.NVML_ERROR_INVALID_ARGUMENT {
				// P-state not supported, skip silently
				continue
			} else if ret != C.NVML_ERROR_NOT_SUPPORTED {
				// Log error but continue resetting other offsets
				if debug != nil && *debug {
					fmt.Printf("DEBUG: Failed to reset clock type %d P-state %d: error %d\n", clockType, pstate, ret)
				}
			}
		}
	}

	return nil
}

// IsNativeClockOffsetSupported checks if native clock offset functionality is available
func IsNativeClockOffsetSupported() bool {
	// Initialize the dynamic loading
	C.init_clock_offset_functions()

	// Check if the function pointers were successfully loaded
	return C.nvmlDeviceGetClockOffsets_ptr != nil && C.nvmlDeviceSetClockOffsets_ptr != nil
}
