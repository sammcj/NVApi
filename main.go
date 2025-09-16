package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const version = "1.3.3"

type GPUInfo struct {
	Index              uint    `json:"index"`
	UUID               string  `json:"uuid"`
	Name               string  `json:"name"`
	GPUUtilisation     uint    `json:"gpu_utilisation"`
	MemoryUtilisation  uint    `json:"memory_utilisation"`
	PowerWatts         uint    `json:"power_watts"`
	PowerLimitWatts    uint    `json:"power_limit_watts"`
	MemoryTotal        float64 `json:"memory_total_gb"`
	MemoryUsed         float64 `json:"memory_used_gb"`
	MemoryFree         float64 `json:"memory_free_gb"`
	MemoryUsagePercent int     `json:"memory_usage_percent"`
	Temperature        uint    `json:"temperature"`
	// FanSpeed           *uint         `json:"fan_speed,omitempty"`
	Processes     []ProcessInfo `json:"processes"`
	PCIeLinkState string        `json:"pcie_link_state"`
	ClockOffsets  *ClockOffsets `json:"clock_offsets,omitempty"`
}

type ProcessInfo struct {
	Pid             uint32   `json:"pid"`
	UsedGpuMemoryMb uint64   `json:"used_gpu_memory_mb"`
	Name            string   `json:"name"`
	Arguments       []string `json:"arguments"`
}

type rateLimiter struct {
	tokens   float64
	capacity float64
	rate     float64
	mu       sync.Mutex
	lastTime time.Time
	cache    *[]GPUInfo
}

// TempPowerLimits holds the temperature-based power limit settings for a GPU
type TempPowerLimits struct {
	LowTemp         int
	MediumTemp      int
	LowTempLimit    uint
	MediumTempLimit uint
	HighTempLimit   uint
}

// ClockOffset represents clock offset information for a performance state
type ClockOffset struct {
	Current int32 `json:"current"` // Current applied offset in MHz
	Min     int32 `json:"min"`     // Minimum allowed offset in MHz
	Max     int32 `json:"max"`     // Maximum allowed offset in MHz
}

// ClockOffsets holds all clock offset information for a GPU
type ClockOffsets struct {
	GPUOffsets map[uint32]ClockOffset `json:"gpu_offsets"` // P-state → GPU offset
	MemOffsets map[uint32]ClockOffset `json:"mem_offsets"` // P-state → Memory offset
	GPURange   *[2]uint32             `json:"gpu_clock_range,omitempty"`
	VRAMRange  *[2]uint32             `json:"vram_clock_range,omitempty"`
}

// ClockOffsetConfig stores the user's desired offset configuration
type ClockOffsetConfig struct {
	GPUOffsets map[uint32]int32 `json:"gpu_offsets"` // P-state → desired offset
	MemOffsets map[uint32]int32 `json:"mem_offsets"` // P-state → desired offset
}

// OffsetRequest represents a request to set clock offsets
type OffsetRequest struct {
	GPUOffsets map[string]int32 `json:"gpu_offsets"` // "P0": 150, "P1": 100
	MemOffsets map[string]int32 `json:"mem_offsets"` // "P0": 500, "P2": -100
}

// OffsetResponse represents current offset status
type OffsetResponse struct {
	Success      bool                   `json:"success"`
	Message      string                 `json:"message,omitempty"`
	ClockOffsets *ClockOffsets          `json:"clock_offsets,omitempty"`
	Applied      map[string]interface{} `json:"applied,omitempty"`
}

// Track GPU capabilities
type GPUCapabilities struct {
	HasFanSpeedSensor bool
	FanSpeedSupported bool
}

const (
	pcieSysfsPath = "/sys/bus/pci/devices"
)

type PCIeConfig struct {
	IdleThresholdSeconds int
	Enabled              bool
	MinSpeed             int
	MaxSpeed             int
}

type PCIeStateManager struct {
	devices            []nvml.Device
	utilisationHistory map[int][]float64
	lastChangeTime     map[int]time.Time
	configs            map[int]*PCIeConfig
	mutex              sync.RWMutex
	updateInterval     time.Duration
	stopChan           chan struct{}
}

var (
	port                      = flag.Int("port", 9999, "Port to listen on")
	rate                      = flag.Int("rate", 3, "Minimum number of seconds between requests")
	debug                     = flag.Bool("debug", false, "Print debug logs to the console")
	help                      = flag.Bool("help", false, "Print this help")
	gpuPowerLimits            map[int]TempPowerLimits
	tempCheckInterval         time.Duration
	lastTempCheckTime         time.Time
	totalPowerCap             uint
	lastTotalPower            uint
	enablePCIeStateManagement = flag.Bool("enable-pcie-state-management", false, "Enable automatic PCIe link state management")
	pcieStateManager          *PCIeStateManager
	gpuClockOffsetConfigs     map[int]ClockOffsetConfig // GPU index → offset config
	lastAppliedOffsets        map[int]ClockOffsets      // Track applied offsets for reporting
	offsetsMutex              sync.RWMutex              // Protect offset operations
	rl                        *rateLimiter              // Rate limiter for HTTP requests
	// gpuCapabilities   []GPUCapabilities
)

// Initialise GPU capabilities
// func initialiseGPUCapabilities() error {
// 	count, ret := nvml.DeviceGetCount()
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
// 	}

// 	gpuCapabilities = make([]GPUCapabilities, count)

// 	for i := 0; i < int(count); i++ {
// 		device, ret := nvml.DeviceGetHandleByIndex(i)
// 		if ret != nvml.SUCCESS {
// 			return fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
// 		}

// 		// Check if fan speed sensor is available
// 		_, ret = device.GetFanSpeed()
// 		gpuCapabilities[i].HasFanSpeedSensor = (ret == nvml.SUCCESS)

// 		// If we have a sensor, check if we can actually read from it
// 		if gpuCapabilities[i].HasFanSpeedSensor {
// 			speed, ret := device.GetFanSpeed()
// 			gpuCapabilities[i].FanSpeedSupported = (ret == nvml.SUCCESS && speed != 0)
// 		}

// 		if *debug {
// 			log.Printf("GPU %d - Has Fan Sensor: %v, Fan Speed Supported: %v",
// 				i, gpuCapabilities[i].HasFanSpeedSensor, gpuCapabilities[i].FanSpeedSupported)
// 		}
// 	}

// 	return nil
// }

// getGPUUUID retrieves the UUID for a given GPU device
func getGPUUUID(device nvml.Device) (string, error) {
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("unable to get UUID for device: %v", nvml.ErrorString(ret))
	}
	return uuid, nil
}

// mapUUIDsToIndices creates a mapping of GPU UUIDs to their corresponding device indices
func mapUUIDsToIndices() (map[string]int, error) {
	uuidToIndex := make(map[string]int)

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	for i := 0; i < int(count); i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
		}

		uuid, err := getGPUUUID(device)
		if err != nil {
			return nil, err
		}

		uuidToIndex[uuid] = i
	}

	return uuidToIndex, nil
}

func parseTotalPowerCap() {
	capStr := os.Getenv("GPU_TOTAL_POWER_CAP")
	if capStr == "" {
		totalPowerCap = 0 // Disabled if not set
		return
	}

	cap, err := strconv.ParseUint(capStr, 10, 32)
	if err != nil {
		log.Printf("Warning: Invalid GPU_TOTAL_POWER_CAP value. Total power cap will be disabled.")
		totalPowerCap = 0
		return
	}

	totalPowerCap = uint(cap)
}

func applyTotalPowerCap(devices []nvml.Device) error {
	if totalPowerCap == 0 {
		return nil // Total power cap is disabled
	}

	var totalPower uint
	var currentPowers []uint
	var maxPowerLimits []uint

	for _, device := range devices {
		power, ret := device.GetPowerUsage()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("unable to get power usage: %v", nvml.ErrorString(ret))
		}
		currentPower := uint(power / 1000) // Convert milliwatts to watts
		totalPower += currentPower
		currentPowers = append(currentPowers, currentPower)

		maxPowerLimit, ret := device.GetPowerManagementLimit()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("unable to get power management limit: %v", nvml.ErrorString(ret))
		}
		maxPowerLimits = append(maxPowerLimits, uint(maxPowerLimit/1000)) // Convert milliwatts to watts
	}

	if totalPower > totalPowerCap {
		log.Printf("Total power consumption (%d W) exceeds the cap (%d W). Adjusting limits...", totalPower, totalPowerCap)

		excessPower := totalPower - totalPowerCap
		for i, device := range devices {
			if currentPowers[i] == 0 {
				continue // Skip GPUs that aren't consuming power
			}

			// Calculate how much this GPU should reduce its power
			reductionRatio := float64(currentPowers[i]) / float64(totalPower)
			reduction := uint(float64(excessPower) * reductionRatio)

			// Ensure we don't reduce below zero
			newLimit := maxPowerLimits[i]
			if reduction < currentPowers[i] {
				newLimit = currentPowers[i] - reduction
			}

			ret := device.SetPowerManagementLimit(uint32(newLimit * 1000)) // Convert watts to milliwatts
			if ret != nvml.SUCCESS {
				return fmt.Errorf("unable to set power management limit for GPU %d: %v", i, nvml.ErrorString(ret))
			}
			log.Printf("GPU %d power limit adjusted to %d W (current consumption: %d W)", i, newLimit, currentPowers[i])
		}
	} else if totalPower >= uint(float64(totalPowerCap)*0.98) && totalPower != lastTotalPower {
		log.Printf("Warning: Total power consumption (%d W) is approaching the cap (%d W)", totalPower, totalPowerCap)
	} else {
		// If we're well below the cap, restore original power limits
		for i, device := range devices {
			ret := device.SetPowerManagementLimit(uint32(maxPowerLimits[i] * 1000)) // Convert watts to milliwatts
			if ret != nvml.SUCCESS {
				return fmt.Errorf("unable to restore power management limit for GPU %d: %v", i, nvml.ErrorString(ret))
			}
		}
	}

	lastTotalPower = totalPower
	return nil
}

func parseTempCheckInterval() {
	intervalStr := os.Getenv("GPU_TEMP_CHECK_INTERVAL")
	if intervalStr == "" {
		tempCheckInterval = 5 * time.Second // Default to 5 seconds if not set
		return
	}

	interval, err := strconv.Atoi(intervalStr)
	if err != nil {
		log.Printf("Warning: Invalid GPU_TEMP_CHECK_INTERVAL value. Using default of 5 seconds.")
		tempCheckInterval = 5 * time.Second
		return
	}

	tempCheckInterval = time.Duration(interval) * time.Second
}
func checkAndApplyPowerLimits() error {
	if time.Since(lastTempCheckTime) < tempCheckInterval {
		return nil
	}

	lastTempCheckTime = time.Now()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	var devices []nvml.Device

	for i := 0; i < int(count); i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
		}

		devices = append(devices, device)

		// Only apply individual temperature-based limits if they are configured
		if _, exists := gpuPowerLimits[i]; exists {
			temperature, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
			if ret != nvml.SUCCESS {
				return fmt.Errorf("unable to get temperature for GPU %d: %v", i, nvml.ErrorString(ret))
			}

			err := applyPowerLimit(device, i, uint(temperature))
			if err != nil {
				log.Printf("Warning: Failed to apply power limit for GPU %d: %v", i, err)
			}
		}
	}

	// Always apply total power cap if it's set, regardless of individual temperature limits
	err := applyTotalPowerCap(devices)
	if err != nil {
		log.Printf("Warning: Failed to apply total power cap: %v", err)
	}

	return nil
}

func (rl *rateLimiter) takeToken() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime)
	rl.lastTime = now

	rl.tokens = math.Min(rl.capacity, rl.tokens+rl.rate*elapsed.Seconds())
	if rl.tokens < 1 {
		return false
	}
	rl.tokens--
	return true
}

func (rl *rateLimiter) getCache() []GPUInfo {
	return *(rl.cache)
}

func getProcessInfo(pid uint32) (string, []string, error) {
	procDir := fmt.Sprintf("/proc/%d", pid)
	cmdlineFile := filepath.Join(procDir, "cmdline")
	cmdline, err := os.ReadFile(cmdlineFile)
	if err != nil {
		return "", nil, err
	}
	cmdlineArgs := strings.Split(string(cmdline), "\x00")
	cmdlineArgs = cmdlineArgs[:len(cmdlineArgs)-1] // remove trailing empty string
	processName := cmdlineArgs[0]
	arguments := cmdlineArgs[1:]
	return processName, arguments, nil
}

// parseTempPowerLimits parses the environment variables for temperature-based power limits
func parseTempPowerLimits() error {
	gpuPowerLimits = make(map[int]TempPowerLimits)

	uuidToIndex, err := mapUUIDsToIndices()
	if err != nil {
		return fmt.Errorf("failed to map UUIDs to indices: %v", err)
	}

	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "GPU_") && strings.Contains(env, "_LOW_TEMP") {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				continue
			}

			key := strings.TrimPrefix(parts[0], "GPU_")
			// Remove all suffixes related to temperature limits
			key = strings.TrimSuffix(key, "_LOW_TEMP")
			key = strings.TrimSuffix(key, "_MEDIUM_TEMP")
			key = strings.TrimSuffix(key, "_HIGH_TEMP")
			key = strings.TrimSuffix(key, "_LOW_TEMP_LIMIT")
			key = strings.TrimSuffix(key, "_MEDIUM_TEMP_LIMIT")
			key = strings.TrimSuffix(key, "_HIGH_TEMP_LIMIT")

			if *debug {
				log.Printf("Debug: Processing key: %s", key)
			}

			var index string
			if strings.Contains(key, "-") {
				// This is a UUID
				if idx, ok := uuidToIndex[key]; ok {
					index = strconv.Itoa(idx)
				} else {
					log.Printf("Warning: Unknown GPU UUID %s", key)
					continue
				}
			} else {
				// This is an index
				index = key
			}

			if *debug {
				log.Printf("Debug: Resolved index: %s", index)
			}

			// Use the key to construct environment variable names
			lowTempEnv := fmt.Sprintf("GPU_%s_LOW_TEMP", key)
			mediumTempEnv := fmt.Sprintf("GPU_%s_MEDIUM_TEMP", key)
			lowTempLimitEnv := fmt.Sprintf("GPU_%s_LOW_TEMP_LIMIT", key)
			mediumTempLimitEnv := fmt.Sprintf("GPU_%s_MEDIUM_TEMP_LIMIT", key)
			highTempLimitEnv := fmt.Sprintf("GPU_%s_HIGH_TEMP_LIMIT", key)

			lowTemp, err := strconv.Atoi(os.Getenv(lowTempEnv))
			if err != nil {
				return fmt.Errorf("invalid LOW_TEMP value for GPU %s: %v", key, err)
			}

			mediumTempStr := os.Getenv(mediumTempEnv)
			if *debug {
				log.Printf("Debug: Looking for MEDIUM_TEMP with key: %s, value: %s", mediumTempEnv, mediumTempStr)
			}

			mediumTemp, err := strconv.Atoi(mediumTempStr)
			if err != nil {
				return fmt.Errorf("invalid MEDIUM_TEMP value for GPU %s: %v", key, err)
			}

			lowTempLimit, err := strconv.ParseUint(os.Getenv(lowTempLimitEnv), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid LOW_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			mediumTempLimit, err := strconv.ParseUint(os.Getenv(mediumTempLimitEnv), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid MEDIUM_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			highTempLimit, err := strconv.ParseUint(os.Getenv(highTempLimitEnv), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid HIGH_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			idx, err := strconv.Atoi(index)
			if err != nil {
				return fmt.Errorf("invalid GPU index %s: %v", index, err)
			}

			if *debug {
				log.Printf("Debug: Successfully parsed all values for GPU %s", key)
			}

			gpuPowerLimits[idx] = TempPowerLimits{
				LowTemp:         lowTemp,
				MediumTemp:      mediumTemp,
				LowTempLimit:    uint(lowTempLimit),
				MediumTempLimit: uint(mediumTempLimit),
				HighTempLimit:   uint(highTempLimit),
			}
		}
	}

	return nil
}

// parseClockOffsetConfig parses environment variables for clock offset configuration
func parseClockOffsetConfig() error {
	gpuClockOffsetConfigs = make(map[int]ClockOffsetConfig)

	if *debug {
		log.Printf("Debug: Starting clock offset configuration parsing...")
	}

	uuidToIndex, err := mapUUIDsToIndices()
	if err != nil {
		return fmt.Errorf("failed to map UUIDs to indices: %v", err)
	}

	if *debug {
		log.Printf("Debug: UUID to Index mapping:")
		for uuid, idx := range uuidToIndex {
			log.Printf("  %s -> %d", uuid, idx)
		}
	}

	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "GPU_") && strings.Contains(env, "_OFFSET_") {
			if *debug {
				log.Printf("Debug: Found clock offset env var: %s", env)
			}
			// Parse patterns like:
			// GPU_UUID_OFFSET_P0_CORE=+150
			// GPU_UUID_OFFSET_P0_MEM=+500
			// GPU_UUID_OFFSET_P2_CORE=-100

			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				continue
			}

			key := parts[0]
			value := parts[1]

			// Extract UUID, P-state, and clock type from key
			if uuid, pstate, clockType, err := parseOffsetEnvKey(key); err == nil {
				if *debug {
					log.Printf("Debug: Parsed key - UUID: %s, P-state: %d, Clock type: %s", uuid, pstate, clockType)
				}
				var idx int
				var exists bool

				if strings.Contains(uuid, "-") {
					// This is a UUID
					idx, exists = uuidToIndex[uuid]
					if !exists {
						log.Printf("Warning: Unknown GPU UUID %s", uuid)
						continue
					}
				} else {
					// This is an index
					idx64, err := strconv.ParseInt(uuid, 10, 32)
					if err != nil {
						log.Printf("Warning: Invalid GPU index %s: %v", uuid, err)
						continue
					}
					idx = int(idx64)
				}

				offset, err := strconv.ParseInt(value, 10, 32)
				if err != nil {
					log.Printf("Invalid offset value for %s: %v", key, err)
					continue
				}

				if _, exists := gpuClockOffsetConfigs[idx]; !exists {
					gpuClockOffsetConfigs[idx] = ClockOffsetConfig{
						GPUOffsets: make(map[uint32]int32),
						MemOffsets: make(map[uint32]int32),
					}
				}

				config := gpuClockOffsetConfigs[idx]
				if clockType == "CORE" {
					config.GPUOffsets[pstate] = int32(offset)
					if *debug {
						log.Printf("Debug: Set GPU P%d offset to %d MHz for device %d", pstate, offset, idx)
					}
				} else if clockType == "MEM" {
					config.MemOffsets[pstate] = int32(offset)
					if *debug {
						log.Printf("Debug: Set Memory P%d offset to %d MHz for device %d", pstate, offset, idx)
					}
				}
				gpuClockOffsetConfigs[idx] = config
			}
		}
	}

	return nil
}

// parseOffsetEnvKey parses environment variable keys like "GPU_UUID_OFFSET_P0_CORE"
func parseOffsetEnvKey(key string) (uuid string, pstate uint32, clockType string, err error) {
	// Expected format: GPU_{UUID}_OFFSET_P{PSTATE}_{CLOCKTYPE}
	parts := strings.Split(key, "_")
	if len(parts) < 5 {
		return "", 0, "", fmt.Errorf("invalid key format")
	}

	// Extract UUID (everything between GPU_ and _OFFSET)
	offsetIndex := -1
	for i, part := range parts {
		if part == "OFFSET" {
			offsetIndex = i
			break
		}
	}

	if offsetIndex == -1 || offsetIndex < 2 {
		return "", 0, "", fmt.Errorf("OFFSET not found in key")
	}

	// Join UUID parts (may contain hyphens)
	uuid = strings.Join(parts[1:offsetIndex], "_")

	// Find P-state (e.g., "P0" → 0)
	var pstateStr string
	var clockTypeIdx int
	for i := offsetIndex + 1; i < len(parts); i++ {
		if strings.HasPrefix(parts[i], "P") && len(parts[i]) > 1 {
			pstateStr = parts[i][1:]
			clockTypeIdx = i + 1
			break
		}
	}

	if pstateStr == "" || clockTypeIdx >= len(parts) {
		return "", 0, "", fmt.Errorf("invalid P-state format")
	}

	pstateVal, err := strconv.ParseUint(pstateStr, 10, 32)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid P-state number: %v", err)
	}

	clockType = parts[clockTypeIdx]
	if clockType != "CORE" && clockType != "MEM" {
		return "", 0, "", fmt.Errorf("invalid clock type: %s", clockType)
	}

	return uuid, uint32(pstateVal), clockType, nil
}

// parsePStateString converts "P0" to 0, "P1" to 1, etc.
func parsePStateString(pstateStr string) uint32 {
	if !strings.HasPrefix(pstateStr, "P") {
		return 0
	}
	pstate, err := strconv.ParseUint(pstateStr[1:], 10, 32)
	if err != nil {
		return 0
	}
	return uint32(pstate)
}

// getClockOffsets retrieves current clock offset information for a device
func getClockOffsets(device nvml.Device, deviceIndex int) (*ClockOffsets, error) {
	// Use native NVML implementation (requires driver 555+)
	return GetClockOffsetsNative(device, deviceIndex)
}

// setClockOffset applies a clock offset to specific P-state using Python script
func setClockOffset(device nvml.Device, clockType string, pstate uint32, offsetMHz int32) error {
	deviceIndex, ret := device.GetIndex()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device index: %v", nvml.ErrorString(ret))
	}

	// Create config for single offset
	config := ClockOffsetConfig{
		GPUOffsets: make(map[uint32]int32),
		MemOffsets: make(map[uint32]int32),
	}

	if clockType == "core" || clockType == "gpu" {
		config.GPUOffsets[pstate] = offsetMHz
	} else if clockType == "mem" || clockType == "memory" {
		config.MemOffsets[pstate] = offsetMHz
	} else {
		return fmt.Errorf("invalid clock type: %s", clockType)
	}

	// Use native NVML implementation (requires driver 555+)
	return SetClockOffsetsNative(device, int(deviceIndex), config)
}

// applyClockOffsets applies all configured offsets for a GPU
func applyClockOffsets(deviceIndex int) error {
	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device %d: %v", deviceIndex, nvml.ErrorString(ret))
	}

	config, exists := gpuClockOffsetConfigs[deviceIndex]
	if !exists {
		return nil // No offsets configured for this GPU
	}

	// Apply GPU core offsets
	for pstate, offset := range config.GPUOffsets {
		if err := setClockOffset(device, "core", pstate, offset); err != nil {
			return fmt.Errorf("failed to set GPU offset %+d MHz for P%d: %v", offset, pstate, err)
		}
		log.Printf("Applied GPU offset %+d MHz to P-state %d on GPU %d", offset, pstate, deviceIndex)
	}

	// Apply memory offsets
	for pstate, offset := range config.MemOffsets {
		if err := setClockOffset(device, "mem", pstate, offset); err != nil {
			return fmt.Errorf("failed to set memory offset %+d MHz for P%d: %v", offset, pstate, err)
		}
		log.Printf("Applied memory offset %+d MHz to P-state %d on GPU %d", offset, pstate, deviceIndex)
	}

	// Cache applied offsets for reporting
	offsetsMutex.Lock()
	if lastAppliedOffsets == nil {
		lastAppliedOffsets = make(map[int]ClockOffsets)
	}

	appliedOffsets := ClockOffsets{
		GPUOffsets: make(map[uint32]ClockOffset),
		MemOffsets: make(map[uint32]ClockOffset),
	}

	for pstate, offset := range config.GPUOffsets {
		appliedOffsets.GPUOffsets[pstate] = ClockOffset{Current: offset, Min: -500, Max: 500}
	}
	for pstate, offset := range config.MemOffsets {
		appliedOffsets.MemOffsets[pstate] = ClockOffset{Current: offset, Min: -1000, Max: 1000}
	}

	lastAppliedOffsets[deviceIndex] = appliedOffsets
	offsetsMutex.Unlock()

	return nil
}

// resetClockOffsets resets all offsets to 0 (stock clocks)
func resetClockOffsets(deviceIndex int) error {
	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device %d: %v", deviceIndex, nvml.ErrorString(ret))
	}

	// Use native NVML implementation (requires driver 555+)
	err := ResetClockOffsetsNative(device, deviceIndex)
	if err != nil {
		return fmt.Errorf("failed to reset clock offsets: %v", err)
	}

	// Clear cached offsets
	offsetsMutex.Lock()
	delete(lastAppliedOffsets, deviceIndex)
	offsetsMutex.Unlock()

	log.Printf("Reset all clock offsets to stock values for GPU %d", deviceIndex)
	return nil
}

// resetToDefaultsWithLogging resets clock offsets to default (0) and logs if non-default values were found
func resetToDefaultsWithLogging(deviceIndex int) error {
	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device %d: %v", deviceIndex, nvml.ErrorString(ret))
	}

	// Get current offsets first to check if they're non-default
	currentOffsets, err := getClockOffsets(device, deviceIndex)
	if err != nil {
		if *debug {
			log.Printf("Warning: Could not get current offsets for GPU %d: %v", deviceIndex, err)
		}
		return nil // Don't fail if we can't read current offsets
	}

	// Check if there are any non-zero offsets to log
	hasNonDefaultOffsets := false
	var nonDefaultOffsets []string

	for pstate, offset := range currentOffsets.GPUOffsets {
		if offset.Current != 0 {
			hasNonDefaultOffsets = true
			nonDefaultOffsets = append(nonDefaultOffsets, fmt.Sprintf("GPU P%d: %+d MHz", pstate, offset.Current))
		}
	}

	for pstate, offset := range currentOffsets.MemOffsets {
		if offset.Current != 0 {
			hasNonDefaultOffsets = true
			nonDefaultOffsets = append(nonDefaultOffsets, fmt.Sprintf("Memory P%d: %+d MHz", pstate, offset.Current))
		}
	}

	if hasNonDefaultOffsets {
		log.Printf("GPU %d: No clock offset configuration found, resetting non-default offsets to 0: %s",
			deviceIndex, strings.Join(nonDefaultOffsets, ", "))
	}

	// Reset to defaults
	err = resetClockOffsets(deviceIndex)
	if err != nil {
		return fmt.Errorf("failed to reset to defaults: %v", err)
	}

	if hasNonDefaultOffsets {
		log.Printf("GPU %d: Successfully reset all clock offsets to default (0)", deviceIndex)
	}

	return nil
}

// validateOffsets ensures offset values are within reasonable ranges
func validateOffsets(req OffsetRequest) error {
	const maxOffset = 1000  // MHz
	const minOffset = -1000 // MHz

	for pstate, offset := range req.GPUOffsets {
		if offset < minOffset || offset > maxOffset {
			return fmt.Errorf("GPU offset %d MHz for %s outside safe range [%d, %d]",
				offset, pstate, minOffset, maxOffset)
		}
	}

	for pstate, offset := range req.MemOffsets {
		if offset < minOffset || offset > maxOffset {
			return fmt.Errorf("memory offset %d MHz for %s outside safe range [%d, %d]",
				offset, pstate, minOffset, maxOffset)
		}
	}

	return nil
}

// checkPythonDependency verifies that Python and required packages are available
func checkClockOffsetSupport() error {
	if !IsNativeClockOffsetSupported() {
		return fmt.Errorf("native clock offset support not available - requires NVIDIA driver 555+")
	}

	if *debug {
		log.Printf("Native clock offset support available")
	}

	return nil
}

// validateDeviceIndex checks if the device index is valid
func validateDeviceIndex(deviceIndex int) error {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	if deviceIndex < 0 || deviceIndex >= int(count) {
		return fmt.Errorf("invalid device index %d. Available devices: 0-%d", deviceIndex, int(count)-1)
	}

	return nil
}

// validatePStateRange checks if P-state values are reasonable
func validatePStateRange(pstate uint32) error {
	if pstate > 7 {
		return fmt.Errorf("P-state %d is outside typical range 0-7", pstate)
	}
	return nil
}

// safeApplyClockOffsets applies offsets with comprehensive error handling
func safeApplyClockOffsets(deviceIndex int) error {
	if err := validateDeviceIndex(deviceIndex); err != nil {
		return fmt.Errorf("device validation failed: %v", err)
	}

	config, exists := gpuClockOffsetConfigs[deviceIndex]
	if !exists {
		// No offsets configured - reset to defaults and check if there were existing offsets
		return resetToDefaultsWithLogging(deviceIndex)
	}

	// Validate all P-states before applying any offsets
	for pstate := range config.GPUOffsets {
		if err := validatePStateRange(pstate); err != nil {
			return fmt.Errorf("GPU offset validation failed: %v", err)
		}
	}

	for pstate := range config.MemOffsets {
		if err := validatePStateRange(pstate); err != nil {
			return fmt.Errorf("memory offset validation failed: %v", err)
		}
	}

	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device %d: %v", deviceIndex, nvml.ErrorString(ret))
	}

	// Use native bulk implementation (requires driver 555+)
	err := SetClockOffsetsNative(device, deviceIndex, config)
	if err == nil {
		// Cache applied offsets for reporting
		offsetsMutex.Lock()
		if lastAppliedOffsets == nil {
			lastAppliedOffsets = make(map[int]ClockOffsets)
		}

		appliedOffsets := ClockOffsets{
			GPUOffsets: make(map[uint32]ClockOffset),
			MemOffsets: make(map[uint32]ClockOffset),
		}

		for pstate, offset := range config.GPUOffsets {
			appliedOffsets.GPUOffsets[pstate] = ClockOffset{Current: offset, Min: -500, Max: 500}
			log.Printf("Applied GPU offset %+d MHz to P-state %d on GPU %d (native)", offset, pstate, deviceIndex)
		}
		for pstate, offset := range config.MemOffsets {
			appliedOffsets.MemOffsets[pstate] = ClockOffset{Current: offset, Min: -1000, Max: 1000}
			log.Printf("Applied memory offset %+d MHz to P-state %d on GPU %d (native)", offset, pstate, deviceIndex)
		}

		lastAppliedOffsets[deviceIndex] = appliedOffsets
		offsetsMutex.Unlock()

		return nil
	}

	// Fallback to individual offset application
	var errors []string

	// Apply GPU core offsets
	for pstate, offset := range config.GPUOffsets {
		if err := setClockOffset(device, "core", pstate, offset); err != nil {
			errors = append(errors, fmt.Sprintf("GPU P%d offset %+d MHz: %v", pstate, offset, err))
		} else {
			log.Printf("Applied GPU offset %+d MHz to P-state %d on GPU %d", offset, pstate, deviceIndex)
		}
	}

	// Apply memory offsets
	for pstate, offset := range config.MemOffsets {
		if err := setClockOffset(device, "mem", pstate, offset); err != nil {
			errors = append(errors, fmt.Sprintf("Memory P%d offset %+d MHz: %v", pstate, offset, err))
		} else {
			log.Printf("Applied memory offset %+d MHz to P-state %d on GPU %d", offset, pstate, deviceIndex)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("some offsets failed to apply: %s", strings.Join(errors, "; "))
	}

	// Cache applied offsets for reporting
	offsetsMutex.Lock()
	if lastAppliedOffsets == nil {
		lastAppliedOffsets = make(map[int]ClockOffsets)
	}

	appliedOffsets := ClockOffsets{
		GPUOffsets: make(map[uint32]ClockOffset),
		MemOffsets: make(map[uint32]ClockOffset),
	}

	for pstate, offset := range config.GPUOffsets {
		appliedOffsets.GPUOffsets[pstate] = ClockOffset{Current: offset, Min: -500, Max: 500}
	}
	for pstate, offset := range config.MemOffsets {
		appliedOffsets.MemOffsets[pstate] = ClockOffset{Current: offset, Min: -1000, Max: 1000}
	}

	lastAppliedOffsets[deviceIndex] = appliedOffsets
	offsetsMutex.Unlock()

	return nil
}

// applyPowerLimit applies the appropriate power limit based on the current temperature
func applyPowerLimit(device nvml.Device, index int, currentTemp uint) error {
	limits, exists := gpuPowerLimits[index]
	if !exists {
		return nil // No limits set for this GPU
	}

	var newLimit uint
	if currentTemp <= uint(limits.LowTemp) {
		newLimit = limits.LowTempLimit
	} else if currentTemp <= uint(limits.MediumTemp) {
		newLimit = limits.MediumTempLimit
	} else {
		newLimit = limits.HighTempLimit
	}

	ret := device.SetPowerManagementLimit(uint32(newLimit * 1000)) // Convert watts to milliwatts
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to set power management limit: %v", nvml.ErrorString(ret))
	}

	return nil
}

// Updated GetGPUInfo function
func GetGPUInfo() ([]GPUInfo, error) {
	var err error
	err = checkAndApplyPowerLimits()
	if err != nil {
		log.Printf("Warning: Failed to check and apply power limits: %v", err)
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	if count == 0 {
		return nil, fmt.Errorf("no devices found")
	}

	gpuInfos := make([]GPUInfo, count)
	for i := 0; i < int(count); i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
		}

		index, ret := device.GetIndex()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get device index: %v", nvml.ErrorString(ret))
		}

		var uuid string
		uuid, err = getGPUUUID(device)
		if err != nil {
			return nil, fmt.Errorf("unable to get device UUID: %v", err)
		}

		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get device name: %v", nvml.ErrorString(ret))
		}

		powerLimitWatts, ret := device.GetPowerManagementLimit()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get power management limit: %v", nvml.ErrorString(ret))
		}

		usage, ret := device.GetUtilizationRates()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get utilisation rates: %v", nvml.ErrorString(ret))
		}

		power, ret := device.GetPowerUsage()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get power usage: %v", nvml.ErrorString(ret))
		}

		memory, ret := device.GetMemoryInfo()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get memory info: %v", nvml.ErrorString(ret))
		}

		temperature, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get temperature: %v", nvml.ErrorString(ret))
		}

		// Apply power limit based on temperature
		err := applyPowerLimit(device, i, uint(temperature))
		if err != nil {
			log.Printf("Warning: Failed to apply power limit for GPU %d: %v", i, err)
		}

		// var fanSpeed *uint
		// if !disableFanSpeed && gpuCapabilities[i].FanSpeedSupported {
		// 	speed, ret := device.GetFanSpeed()
		// 	if ret == nvml.SUCCESS {
		// 		fanSpeedUint := uint(speed)
		// 		fanSpeed = &fanSpeedUint
		// 	} else {
		// 		// Log the issue but don't return an error
		// 		if *debug {
		// 			log.Printf("Warning: Failed to get fan speed for GPU %d: %v", i, nvml.ErrorString(ret))
		// 		}
		// 	}
		// }

		processes, ret := nvml.DeviceGetComputeRunningProcesses(device)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get running processes: %v", nvml.ErrorString(ret))
		}

		processesInfo := make([]ProcessInfo, len(processes))
		for j, process := range processes {
			processName, arguments, err := getProcessInfo(process.Pid)
			if err != nil {
				return nil, err
			}
			processesInfo[j] = ProcessInfo{
				Pid:             process.Pid,
				UsedGpuMemoryMb: process.UsedGpuMemory / 1024 / 1024,
				Name:            processName,
				Arguments:       arguments,
			}
		}

		memoryTotal := float64(memory.Total) / 1024 / 1024 / 1024
		memoryUsed := float64(memory.Used) / 1024 / 1024 / 1024
		memoryFree := float64(memory.Free) / 1024 / 1024 / 1024
		memoryUsagePercent := int(math.Round((float64(memory.Used) / float64(memory.Total)) * 100))

		devices := make([]nvml.Device, count)
		for i := 0; i < int(count); i++ {
			device, ret := nvml.DeviceGetHandleByIndex(i)
			if ret != nvml.SUCCESS {
				log.Fatalf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
			}
			devices[i] = device
		}
		var pcieLinkState string
		if pcieStateManager != nil {
			state, err := pcieStateManager.GetCurrentLinkState(i)
			if err != nil {
				log.Printf("Warning: Failed to get PCIe link state for GPU %d: %v", i, err)
				pcieLinkState = "unknown"
			} else {
				pcieLinkState = state
			}
		} else {
			pcieLinkState = "not managed"
		}

		// Add clock offset information
		var clockOffsets *ClockOffsets
		if offsets, err := getClockOffsets(device, i); err == nil {
			clockOffsets = offsets
		}

		gpuInfo := GPUInfo{
			Index:              uint(index),
			UUID:               uuid,
			Name:               name,
			GPUUtilisation:     uint(usage.Gpu),
			MemoryUtilisation:  uint(usage.Memory),
			MemoryTotal:        math.Round(memoryTotal*100) / 100,
			MemoryUsed:         math.Round(memoryUsed*100) / 100,
			MemoryFree:         math.Round(memoryFree*100) / 100,
			MemoryUsagePercent: memoryUsagePercent,
			Temperature:        uint(temperature),
			// FanSpeed:           fanSpeed,
			PowerWatts:      uint(math.Round(float64(power) / 1000)),
			PowerLimitWatts: uint(math.Round(float64(powerLimitWatts) / 1000)),
			Processes:       processesInfo,
			PCIeLinkState:   pcieLinkState,
			ClockOffsets:    clockOffsets,
		}

		gpuInfos[i] = gpuInfo
	}

	return gpuInfos, nil
}

// extractGPUIndex extracts GPU index from URL path like "/gpu/0/offsets"
func extractGPUIndex(path string) (int, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid path format")
	}

	return strconv.Atoi(parts[1])
}

// handleOriginalGPURoute handles the original GPU endpoints functionality
func handleOriginalGPURoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pathToField := map[string]func(*GPUInfo) interface{}{
		"/gpu/index":                func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Index },
		"/gpu/uuid":                 func(gpuInfo *GPUInfo) interface{} { return gpuInfo.UUID },
		"/gpu/name":                 func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Name },
		"/gpu/gpu_utilisation":      func(gpuInfo *GPUInfo) interface{} { return gpuInfo.GPUUtilisation },
		"/gpu/memory_utilisation":   func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUtilisation },
		"/gpu/power_watts":          func(gpuInfo *GPUInfo) interface{} { return gpuInfo.PowerWatts },
		"/gpu/power_limit_watts":    func(gpuInfo *GPUInfo) interface{} { return gpuInfo.PowerLimitWatts },
		"/gpu/memory_total_gb":      func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryTotal },
		"/gpu/memory_used_gb":       func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUsed },
		"/gpu/memory_free_gb":       func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryFree },
		"/gpu/memory_usage_percent": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUsagePercent },
		"/gpu/temperature":          func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Temperature },
		// "/gpu/fan_speed":            func(gpuInfo *GPUInfo) interface{} { return gpuInfo.FanSpeed },
		"/gpu/all":       func(gpuInfo *GPUInfo) interface{} { return gpuInfo },
		"/gpu/processes": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Processes },
	}

	if *debug {
		fmt.Println("Path: ", r.URL.Path)
		fmt.Println("Request: ", r)
	}

	if !rl.takeToken() {
		json.NewEncoder(w).Encode(rl.getCache())
		return
	}

	gpuInfos, err := GetGPUInfo()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	*rl.cache = gpuInfos

	for _, gpuInfo := range gpuInfos {
		path := r.URL.Path
		if f, ok := pathToField[path]; ok {
			json.NewEncoder(w).Encode(f(&gpuInfo))
			return
		}
	}

	// If no specific field is matched, return all GPU info
	json.NewEncoder(w).Encode(gpuInfos)
}

// handleGPURoutes routes between offset endpoints and regular GPU endpoints
func handleGPURoutes(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Check if this is an offset-related route
	if strings.Contains(path, "/offsets") {
		handleGPUOffsetRoutes(w, r)
		return
	}

	// Otherwise, handle as regular GPU endpoint
	handleOriginalGPURoute(w, r)
}

// handleGPUOffsetRoutes routes clock offset API requests
func handleGPUOffsetRoutes(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Parse routes like:
	// GET  /gpu/0/offsets
	// POST /gpu/0/offsets
	// POST /gpu/0/offsets/reset
	// GET  /gpu/0/offsets/ranges

	if strings.HasSuffix(path, "/offsets/reset") {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleResetOffsets(w, r)
		return
	}

	if strings.HasSuffix(path, "/offsets/ranges") {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleGetOffsetRanges(w, r)
		return
	}

	if strings.HasSuffix(path, "/offsets") {
		switch r.Method {
		case "GET":
			handleGetOffsets(w, r)
		case "POST":
			handleSetOffsets(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// If no offset-related endpoint matched, fall back to original GPU handler
	http.NotFound(w, r)
}

// handleGetOffsets returns current clock offset information
func handleGetOffsets(w http.ResponseWriter, r *http.Request) {
	deviceIndex, err := extractGPUIndex(r.URL.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid GPU index: %v", err), http.StatusBadRequest)
		return
	}

	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		http.Error(w, fmt.Sprintf("Unable to get device: %v", nvml.ErrorString(ret)),
			http.StatusInternalServerError)
		return
	}

	offsets, err := getClockOffsets(device, deviceIndex)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to get offsets: %v", err),
			http.StatusInternalServerError)
		return
	}

	response := OffsetResponse{
		Success:      true,
		ClockOffsets: offsets,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleSetOffsets applies new clock offset configuration
func handleSetOffsets(w http.ResponseWriter, r *http.Request) {
	deviceIndex, err := extractGPUIndex(r.URL.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid GPU index: %v", err), http.StatusBadRequest)
		return
	}

	var req OffsetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate offset ranges
	if err := validateOffsets(req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid offsets: %v", err), http.StatusBadRequest)
		return
	}

	// Convert string P-states to uint32
	config := ClockOffsetConfig{
		GPUOffsets: make(map[uint32]int32),
		MemOffsets: make(map[uint32]int32),
	}

	for pstateStr, offset := range req.GPUOffsets {
		pstate := parsePStateString(pstateStr)
		config.GPUOffsets[pstate] = offset
	}

	for pstateStr, offset := range req.MemOffsets {
		pstate := parsePStateString(pstateStr)
		config.MemOffsets[pstate] = offset
	}

	// Store configuration and apply
	gpuClockOffsetConfigs[deviceIndex] = config

	if err := safeApplyClockOffsets(deviceIndex); err != nil {
		http.Error(w, fmt.Sprintf("Failed to apply offsets: %v", err),
			http.StatusInternalServerError)
		return
	}

	response := OffsetResponse{
		Success: true,
		Message: "Clock offsets applied successfully",
		Applied: map[string]interface{}{
			"gpu_offsets": req.GPUOffsets,
			"mem_offsets": req.MemOffsets,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleResetOffsets resets all offsets to stock values
func handleResetOffsets(w http.ResponseWriter, r *http.Request) {
	deviceIndex, err := extractGPUIndex(r.URL.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid GPU index: %v", err), http.StatusBadRequest)
		return
	}

	if err := resetClockOffsets(deviceIndex); err != nil {
		http.Error(w, fmt.Sprintf("Failed to reset offsets: %v", err),
			http.StatusInternalServerError)
		return
	}

	// Clear stored configuration
	delete(gpuClockOffsetConfigs, deviceIndex)

	response := OffsetResponse{
		Success: true,
		Message: "Clock offsets reset to stock values",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleGetOffsetRanges returns supported offset ranges
func handleGetOffsetRanges(w http.ResponseWriter, r *http.Request) {
	deviceIndex, err := extractGPUIndex(r.URL.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid GPU index: %v", err), http.StatusBadRequest)
		return
	}

	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		http.Error(w, fmt.Sprintf("Unable to get device: %v", nvml.ErrorString(ret)),
			http.StatusInternalServerError)
		return
	}

	offsets, err := getClockOffsets(device, deviceIndex)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to get offset ranges: %v", err),
			http.StatusInternalServerError)
		return
	}

	ranges := map[string]interface{}{
		"gpu_offset_ranges": offsets.GPUOffsets,
		"mem_offset_ranges": offsets.MemOffsets,
		"gpu_clock_range":   offsets.GPURange,
		"vram_clock_range":  offsets.VRAMRange,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ranges)
}

func main() {
	println("NVApi Version: ", version)
	flag.Parse()

	// if DEBUG=true, set debug to true
	if os.Getenv("DEBUG") == "true" {
		*debug = true
	}

	if *debug {
		println("*** Debug Mode ***")
		println("Port: ", *port)
		println("Rate: ", *rate)
		println("***            ***")
	}

	if *port < 1 || *rate < 1 {
		flag.Usage()
		return
	}

	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		log.Fatalf("unable to initialise NVML: %v", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()

	count, err := nvml.DeviceGetCount()
	if err != nvml.SUCCESS || count == 0 {
		log.Fatalf("no devices found")
	}

	if *help {
		flag.Usage()
		return
	}

	// Initialise GPU capabilities
	// if err := initialiseGPUCapabilities(); err != nil {
	// 	log.Fatalf("Failed to initialise GPU capabilities: %v", err)
	// }

	// Initialise PCIeStateManager
	var pcieStateManagerInitialised bool
	if *enablePCIeStateManagement {
		log.Println("Initializing PCIe State Manager")
		devices := make([]nvml.Device, count)
		for i := 0; i < int(count); i++ {
			device, ret := nvml.DeviceGetHandleByIndex(i)
			if ret != nvml.SUCCESS {
				log.Fatalf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
			}
			devices[i] = device
		}
		pcieStateManager = NewPCIeStateManager(devices)
		pcieStateManager.Start()
		defer pcieStateManager.Stop()
		pcieStateManagerInitialised = true
		log.Println("PCIe State Manager initialized and started")
	} else {
		log.Println("PCIe State Management is disabled")
	}

	// Print any configured power limits
	for i, limits := range gpuPowerLimits {
		log.Printf("GPU %d power limits: LowTemp: %d, LowTempLimit: %d W, MediumTemp: %d, MediumTempLimit: %d W, HighTempLimit: %d W",
			i, limits.LowTemp, limits.LowTempLimit, limits.MediumTemp, limits.MediumTempLimit, limits.HighTempLimit)
	}

	// Parse temperature-based power limits from environment variables
	if err := parseTempPowerLimits(); err != nil {
		log.Fatalf("Error parsing temperature-based power limits: %v", err)
	}

	if *debug {
		// Print any configured power limits
		for index, limits := range gpuPowerLimits {
			log.Printf("GPU %d power limits: LowTemp: %d, LowTempLimit: %d W, MediumTemp: %d, MediumTempLimit: %d W, HighTempLimit: %d W",
				index, limits.LowTemp, limits.LowTempLimit, limits.MediumTemp, limits.MediumTempLimit, limits.HighTempLimit)
		}
	}

	// Parse temperature check interval from environment variable
	parseTempCheckInterval()

	// Parse total power cap from environment variable
	parseTotalPowerCap()

	// Parse clock offset configuration from environment variables
	if err := parseClockOffsetConfig(); err != nil {
		log.Fatalf("Error parsing clock offset configuration: %v", err)
	}

	// Log configured offsets
	if len(gpuClockOffsetConfigs) > 0 {
		for deviceIndex, config := range gpuClockOffsetConfigs {
			log.Printf("GPU %d offset configuration:", deviceIndex)
			for pstate, offset := range config.GPUOffsets {
				log.Printf("  P%d GPU: %+d MHz", pstate, offset)
			}
			for pstate, offset := range config.MemOffsets {
				log.Printf("  P%d Memory: %+d MHz", pstate, offset)
			}
		}
	} else {
		log.Printf("Clock offset configuration: No offsets configured (all GPUs will be reset to defaults)")
	}

	// Check Python dependency if clock offsets are configured
	if len(gpuClockOffsetConfigs) > 0 {
		if err := checkClockOffsetSupport(); err != nil {
			log.Printf("Warning: Clock offset functionality disabled: %v", err)
			gpuClockOffsetConfigs = make(map[int]ClockOffsetConfig) // Clear configs
		}
	}

	// Apply initial offset configuration or reset to defaults
	if len(gpuClockOffsetConfigs) > 0 {
		// Apply configured offsets
		for deviceIndex := range gpuClockOffsetConfigs {
			if err := safeApplyClockOffsets(deviceIndex); err != nil {
				log.Printf("Warning: Failed to apply initial offsets for GPU %d: %v", deviceIndex, err)
			}
		}
	} else {
		// No configurations found - check all GPUs and reset any non-default offsets
		for i := 0; i < int(count); i++ {
			if err := safeApplyClockOffsets(i); err != nil {
				log.Printf("Warning: Failed to check/reset offsets for GPU %d: %v", i, err)
			}
		}
	}

	rl = &rateLimiter{
		capacity: float64(1),
		rate:     float64(*rate) / 3600,
		cache:    new([]GPUInfo),
	}

	// Start a goroutine to periodically update the GPU info
	go func() {
		for {
			gpuInfos, err := GetGPUInfo()
			if err != nil {
				if !strings.Contains(err.Error(), "fan speed") {
					log.Printf("Error updating GPU info: %v", err)
				}
			} else {
				rl.mu.Lock()
				*rl.cache = gpuInfos
				rl.mu.Unlock()

				if pcieStateManagerInitialised {
					pcieStateManager.UpdateUtilisation()
				}
			}
			time.Sleep(time.Duration(*rate) * time.Second)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !rl.takeToken() {
			rl.mu.Lock()
			json.NewEncoder(w).Encode(rl.cache)
			rl.mu.Unlock()
			return
		}

		rl.mu.Lock()
		json.NewEncoder(w).Encode(rl.cache)
		rl.mu.Unlock()

		if *debug {
			fmt.Println("GPU Info: ", rl.cache)
		}
	})

	// GPU endpoints (both original and clock offset management)
	http.HandleFunc("/gpu", handleOriginalGPURoute) // For exact /gpu routes
	http.HandleFunc("/gpu/", handleGPURoutes)       // For /gpu/* routes

	log.Printf("Server starting on port %d", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

func NewPCIeStateManager(devices []nvml.Device) *PCIeStateManager {
	manager := &PCIeStateManager{
		devices:            devices,
		utilisationHistory: make(map[int][]float64),
		lastChangeTime:     make(map[int]time.Time),
		configs:            make(map[int]*PCIeConfig),
		updateInterval:     time.Second, // Update every second
		stopChan:           make(chan struct{}),
	}
	manager.loadConfigurations()

	// Initialise utilization history for each GPU
	for i := range devices {
		manager.utilisationHistory[i] = make([]float64, 0, manager.configs[i].IdleThresholdSeconds)
	}

	return manager
}

func (m *PCIeStateManager) Start() {
	go m.updateLoop()
}

func (m *PCIeStateManager) Stop() {
	close(m.stopChan)
}

func (m *PCIeStateManager) updateLoop() {
	ticker := time.NewTicker(m.updateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.UpdateUtilisation()
		case <-m.stopChan:
			return
		}
	}
}

func setPCIeSpeed(device string, targetSpeed int) error {
	// Ensure the device path is correct
	if !strings.HasPrefix(device, "0000:") {
		device = "0000:" + device
	}

	// Get device capabilities
	capExp, err := runSetpci(device, "CAP_EXP+02.W")
	if err != nil {
		return fmt.Errorf("failed to get device capabilities: %v", err)
	}

	pt, err := strconv.ParseInt(capExp, 16, 64)
	if err != nil {
		return fmt.Errorf("failed to parse device capabilities: %v", err)
	}
	pt = (pt & 0xF0) >> 4

	// Adjust device if necessary
	if pt == 0 || pt == 1 || pt == 5 {
		output, err := exec.Command("bash", "-c", fmt.Sprintf("basename $(dirname $(readlink /sys/bus/pci/devices/%s))", device)).Output()
		if err != nil {
			return fmt.Errorf("failed to get parent device: %v", err)
		}
		device = strings.TrimSpace(string(output))
	}

	// Get link capabilities and current status
	lc, err := runSetpci(device, "CAP_EXP+0c.L")
	if err != nil {
		return fmt.Errorf("failed to get link capabilities: %v", err)
	}

	ls, err := runSetpci(device, "CAP_EXP+12.W")
	if err != nil {
		return fmt.Errorf("failed to get link status: %v", err)
	}

	maxSpeed, err := strconv.ParseInt(lc[len(lc)-1:], 16, 64)
	if err != nil {
		return fmt.Errorf("failed to parse max link speed: %v", err)
	}

	currentSpeed, err := strconv.ParseInt(ls[len(ls)-1:], 16, 64)
	if err != nil {
		return fmt.Errorf("failed to parse current link speed: %v", err)
	}

	log.Printf("Device: %s, Max Speed: %d, Current Speed: %d, Target Speed: %d", device, maxSpeed, currentSpeed, targetSpeed)

	// Adjust target speed if necessary
	if targetSpeed > int(maxSpeed) {
		targetSpeed = int(maxSpeed)
	}

	// Set new link speed
	lc2, err := runSetpci(device, "CAP_EXP+30.L")
	if err != nil {
		return fmt.Errorf("failed to get link control 2: %v", err)
	}

	lc2Int, err := strconv.ParseInt(lc2, 16, 64)
	if err != nil {
		return fmt.Errorf("failed to parse link control 2: %v", err)
	}

	lc2New := fmt.Sprintf("%08x", (lc2Int&0xFFFFFFF0)|int64(targetSpeed))

	_, err = runSetpci(device, fmt.Sprintf("CAP_EXP+30.L=%s", lc2New))
	if err != nil {
		return fmt.Errorf("failed to set new link speed: %v", err)
	}

	// Trigger link retraining
	_, err = runSetpci(device, "CAP_EXP+10.L=20:20")
	if err != nil {
		return fmt.Errorf("failed to trigger link retraining: %v", err)
	}

	// Wait for retraining
	time.Sleep(100 * time.Millisecond)

	// Get new link status
	ls, err = runSetpci(device, "CAP_EXP+12.W")
	if err != nil {
		return fmt.Errorf("failed to get new link status: %v", err)
	}

	newSpeed, err := strconv.ParseInt(ls[len(ls)-1:], 16, 64)
	if err != nil {
		return fmt.Errorf("failed to parse new link speed: %v", err)
	}

	log.Printf("Device: %s, New Speed: %d", device, newSpeed)

	return nil
}

func runSetpci(device, command string) (string, error) {
	output, err := exec.Command("setpci", "-s", device, command).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (m *PCIeStateManager) loadConfigurations() {
	for i := range m.devices {
		m.configs[i] = &PCIeConfig{
			IdleThresholdSeconds: getEnvAsInt(fmt.Sprintf("GPU_%d_PCIE_IDLE_THRESHOLD", i), 20),
			Enabled:              getEnvAsBool(fmt.Sprintf("GPU_%d_PCIE_MANAGEMENT_ENABLED", i), false),
			MinSpeed:             getEnvAsInt(fmt.Sprintf("GPU_%d_PCIE_MIN_SPEED", i), 1),
			MaxSpeed:             getEnvAsInt(fmt.Sprintf("GPU_%d_PCIE_MAX_SPEED", i), 16), // Assuming PCIe 5.0 as max
		}
	}

	// Global configuration (applied to all GPUs if not set individually)
	globalThreshold := getEnvAsInt("GPU_PCIE_IDLE_THRESHOLD", 20)
	globalEnabled := getEnvAsBool("GPU_PCIE_MANAGEMENT_ENABLED", false)
	globalMinSpeed := getEnvAsInt("GPU_PCIE_MIN_SPEED", 1)
	globalMaxSpeed := getEnvAsInt("GPU_PCIE_MAX_SPEED", 16)

	for i, config := range m.configs {
		if !config.Enabled {
			config.Enabled = globalEnabled
		}
		if config.IdleThresholdSeconds == 20 {
			config.IdleThresholdSeconds = globalThreshold
		}
		if config.MinSpeed == 1 {
			config.MinSpeed = globalMinSpeed
		}
		if config.MaxSpeed == 16 {
			config.MaxSpeed = globalMaxSpeed
		}
		log.Printf("GPU %d PCIe management: enabled=%v, idle threshold=%d seconds, min speed=%d, max speed=%d",
			i, config.Enabled, config.IdleThresholdSeconds, config.MinSpeed, config.MaxSpeed)
	}
}
func (m *PCIeStateManager) UpdateUtilisation() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	log.Println("Starting UpdateUtilisation")

	for i, device := range m.devices {
		if !m.configs[i].Enabled {
			log.Printf("GPU %d: PCIe management not enabled, skipping", i)
			continue
		}

		usage, ret := device.GetUtilizationRates()
		if ret != nvml.SUCCESS {
			log.Printf("Failed to get utilization for GPU %d: %v", i, nvml.ErrorString(ret))
			continue
		}

		m.utilisationHistory[i] = append(m.utilisationHistory[i], float64(usage.Gpu))
		if len(m.utilisationHistory[i]) > m.configs[i].IdleThresholdSeconds {
			m.utilisationHistory[i] = m.utilisationHistory[i][1:]
		}

		log.Printf("GPU %d utilization: %.2f%% (History length: %d)", i, float64(usage.Gpu), len(m.utilisationHistory[i]))

		if m.shouldChangeLinkSpeed(i) {
			log.Printf("GPU %d: Attempting to change link speed", i)
			err := m.changeLinkSpeed(i, device)
			if err != nil {
				log.Printf("Failed to change link speed for GPU %d: %v", i, err)
			} else {
				log.Printf("GPU %d: Successfully changed link speed", i)
			}
		} else {
			log.Printf("GPU %d: No need to change link speed", i)
		}
	}

	log.Println("Finished UpdateUtilisation")
}

func (m *PCIeStateManager) shouldChangeLinkSpeed(index int) bool {
	history := m.utilisationHistory[index]
	config := m.configs[index]

	log.Printf("GPU %d: Checking if link speed should change", index)
	log.Printf("GPU %d: History length: %d, Required: %d", index, len(history), config.IdleThresholdSeconds)

	if len(history) < config.IdleThresholdSeconds {
		log.Printf("GPU %d: Not enough history to determine if link speed should change", index)
		return false
	}

	isIdle := true
	for _, util := range history {
		if util >= 1.0 {
			isIdle = false
			break
		}
	}

	log.Printf("GPU %d: Is idle? %v", index, isIdle)

	if isIdle {
		lastChange, exists := m.lastChangeTime[index]
		if !exists || time.Since(lastChange) >= 5*time.Minute {
			log.Printf("GPU %d: Conditions met to change link speed (idle for %d seconds)", index, config.IdleThresholdSeconds)
			return true
		}
		log.Printf("GPU %d: Idle, but too soon since last change (%.2f minutes ago)", index, time.Since(lastChange).Minutes())
	} else {
		log.Printf("GPU %d: Not idle (utilization above threshold in last %d seconds)", index, config.IdleThresholdSeconds)
	}

	return false
}

func (m *PCIeStateManager) changeLinkSpeed(index int, device nvml.Device) error {
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	deviceAddress := fmt.Sprintf("%04x:%02x:%02x.0", pciInfo.Domain, pciInfo.Bus, pciInfo.Device)

	currentSpeed, err := m.GetCurrentLinkSpeed(index)
	if err != nil {
		return fmt.Errorf("failed to get current link speed for GPU %d: %v", index, err)
	}

	targetSpeed := m.configs[index].MinSpeed
	if currentSpeed == m.configs[index].MinSpeed {
		targetSpeed = m.configs[index].MaxSpeed
	}

	log.Printf("GPU %d: Attempting to change PCIe link speed from %d to %d", index, currentSpeed, targetSpeed)

	err = setPCIeSpeed(deviceAddress, targetSpeed)
	if err != nil {
		return fmt.Errorf("failed to set PCIe speed for GPU %d: %v", index, err)
	}

	log.Printf("GPU %d: Successfully changed PCIe link speed to %d", index, targetSpeed)
	m.lastChangeTime[index] = time.Now()

	return nil
}

func (m *PCIeStateManager) GetCurrentLinkSpeed(index int) (int, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.configs[index].Enabled {
		return 0, fmt.Errorf("PCIe management not enabled for GPU %d", index)
	}

	device := m.devices[index]
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	deviceAddress := fmt.Sprintf("%04x:%02x:%02x.0", pciInfo.Domain, pciInfo.Bus, pciInfo.Device)

	ls, err := runSetpci(deviceAddress, "CAP_EXP+12.W")
	if err != nil {
		return 0, fmt.Errorf("failed to get link status for GPU %d: %v", index, err)
	}

	speed, err := strconv.ParseInt(ls[len(ls)-1:], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse link speed for GPU %d: %v", index, err)
	}

	return int(speed), nil
}

func (m *PCIeStateManager) GetCurrentLinkState(index int) (string, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.configs[index].Enabled {
		return "not managed", nil
	}

	device := m.devices[index]
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	domainBus := fmt.Sprintf("%04x:%02x:%02x.0", pciInfo.Domain, pciInfo.Bus, pciInfo.Device)
	powerControlFile := filepath.Join(pcieSysfsPath, domainBus, "power", "control")

	content, err := os.ReadFile(powerControlFile)
	if err != nil {
		return "", fmt.Errorf("failed to read power control for GPU %d: %v", index, err)
	}

	return strings.TrimSpace(string(content)), nil
}

// Utility functions for environment variable parsing
func getEnvAsInt(name string, defaultVal int) int {
	valStr := os.Getenv(name)
	if value, err := strconv.Atoi(valStr); err == nil {
		return value
	}
	return defaultVal
}

func getEnvAsBool(name string, defaultVal bool) bool {
	valStr := os.Getenv(name)
	if value, err := strconv.ParseBool(valStr); err == nil {
		return value
	}
	return defaultVal
}
