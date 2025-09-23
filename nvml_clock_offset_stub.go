//go:build !linux
// +build !linux

package main

import (
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// GetClockOffsetsNative retrieves clock offsets using native NVML calls (Linux only)
func GetClockOffsetsNative(device nvml.Device, deviceIndex int) (*ClockOffsets, error) {
	return nil, fmt.Errorf("native clock offset functionality is only available on Linux")
}

// SetClockOffsetsNative sets clock offsets using native NVML calls (Linux only)
func SetClockOffsetsNative(device nvml.Device, deviceIndex int, config ClockOffsetConfig) error {
	return fmt.Errorf("native clock offset functionality is only available on Linux")
}

// ResetClockOffsetsNative resets clock offsets to 0 using native NVML calls (Linux only)
func ResetClockOffsetsNative(device nvml.Device, deviceIndex int) error {
	return fmt.Errorf("native clock offset functionality is only available on Linux")
}

// IsNativeClockOffsetSupported checks if native clock offset functionality is available (Linux only)
func IsNativeClockOffsetSupported() bool {
	return false
}