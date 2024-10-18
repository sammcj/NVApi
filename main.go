package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
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

const version = "1.3.2"

type GPUInfo struct {
	Index              uint    `json:"index"`
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

			log.Printf("Debug: Processing key: %s", key)

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

			log.Printf("Debug: Resolved index: %s", index)

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
			log.Printf("Debug: Looking for MEDIUM_TEMP with key: %s, value: %s", mediumTempEnv, mediumTempStr)

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

			log.Printf("Debug: Successfully parsed all values for GPU %s", key)

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
	err := checkAndApplyPowerLimits()
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

		gpuInfo := GPUInfo{
			Index:              uint(index),
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
		}

		gpuInfos[i] = gpuInfo
	}

	return gpuInfos, nil
}

func main() {
	println("NVApi Version: ", version)
	flag.Parse()

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
	var pcieStateManagerInitialized bool
	if *enablePCIeStateManagement {
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
		pcieStateManagerInitialized = true
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

	rl := &rateLimiter{
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

				if pcieStateManagerInitialized {
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

	http.HandleFunc("/gpu", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		pathToField := map[string]func(*GPUInfo) interface{}{
			"/gpu/index":                func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Index },
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
	})

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

	for i, device := range m.devices {
		if !m.configs[i].Enabled {
			continue
		}

		usage, ret := device.GetUtilizationRates()
		if ret != nvml.SUCCESS {
			log.Printf("Failed to get utilization for GPU %d: %v", i, nvml.ErrorString(ret))
			continue
		}

		// Append new utilization data
		m.utilisationHistory[i] = append(m.utilisationHistory[i], float64(usage.Gpu))

		// Trim history if it exceeds the threshold
		if len(m.utilisationHistory[i]) > m.configs[i].IdleThresholdSeconds {
			m.utilisationHistory[i] = m.utilisationHistory[i][len(m.utilisationHistory[i])-m.configs[i].IdleThresholdSeconds:]
		}

		log.Printf("GPU %d utilization: %.2f%% (History length: %d)", i, float64(usage.Gpu), len(m.utilisationHistory[i]))

		if m.shouldChangeLinkSpeed(i) {
			err := m.changeLinkSpeed(i, device)
			if err != nil {
				log.Printf("Failed to change link speed for GPU %d: %v", i, err)
			}
		}
	}
}

func (m *PCIeStateManager) shouldChangeLinkSpeed(index int) bool {
	history := m.utilisationHistory[index]
	config := m.configs[index]

	if len(history) < config.IdleThresholdSeconds {
		log.Printf("GPU %d: Not enough history to determine if link speed should change (current: %d, required: %d)",
			index, len(history), config.IdleThresholdSeconds)
		return false
	}

	isIdle := true
	for _, util := range history {
		if util >= 1.0 {
			isIdle = false
			break
		}
	}

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

	content, err := ioutil.ReadFile(powerControlFile)
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
