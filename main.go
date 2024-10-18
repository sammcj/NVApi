package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const version = "1.3.2"

type GPUInfo struct {
	Index              uint          `json:"index"`
	Name               string        `json:"name"`
	GPUUtilisation     uint          `json:"gpu_utilisation"`
	MemoryUtilisation  uint          `json:"memory_utilisation"`
	PowerWatts         uint          `json:"power_watts"`
	PowerLimitWatts    uint          `json:"power_limit_watts"`
	MemoryTotal        float64       `json:"memory_total_gb"`
	MemoryUsed         float64       `json:"memory_used_gb"`
	MemoryFree         float64       `json:"memory_free_gb"`
	MemoryUsagePercent int           `json:"memory_usage_percent"`
	Temperature        uint          `json:"temperature"`
	FanSpeed           uint          `json:"fan_speed"`
	Processes          []ProcessInfo `json:"processes"`
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

var (
	port              = flag.Int("port", 9999, "Port to listen on")
	rate              = flag.Int("rate", 3, "Minimum number of seconds between requests")
	debug             = flag.Bool("debug", false, "Print debug logs to the console")
	help              = flag.Bool("help", false, "Print this help")
	gpuPowerLimits    map[int]TempPowerLimits
	tempCheckInterval time.Duration
	lastTempCheckTime time.Time
	totalPowerCap     uint
	lastTotalPower    uint
)

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
			key = strings.TrimSuffix(key, "_LOW_TEMP")

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

			lowTemp, err := strconv.Atoi(parts[1])
			if err != nil {
				return fmt.Errorf("invalid LOW_TEMP value for GPU %s: %v", key, err)
			}

			mediumTemp, err := strconv.Atoi(os.Getenv(fmt.Sprintf("GPU_%s_MEDIUM_TEMP", key)))
			if err != nil {
				return fmt.Errorf("invalid MEDIUM_TEMP value for GPU %s: %v", key, err)
			}

			lowTempLimit, err := strconv.ParseUint(os.Getenv(fmt.Sprintf("GPU_%s_LOW_TEMP_LIMIT", key)), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid LOW_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			mediumTempLimit, err := strconv.ParseUint(os.Getenv(fmt.Sprintf("GPU_%s_MEDIUM_TEMP_LIMIT", key)), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid MEDIUM_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			highTempLimit, err := strconv.ParseUint(os.Getenv(fmt.Sprintf("GPU_%s_HIGH_TEMP_LIMIT", key)), 10, 32)
			if err != nil {
				return fmt.Errorf("invalid HIGH_TEMP_LIMIT value for GPU %s: %v", key, err)
			}

			idx, err := strconv.Atoi(index)
			if err != nil {
				return fmt.Errorf("invalid GPU index %s: %v", index, err)
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

		PowerLimitWatts, ret := device.GetPowerManagementLimit()
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

		fanSpeed, ret := device.GetFanSpeed()
		if ret != nvml.SUCCESS {
			// return nil, fmt.Errorf("unable to get fan speed: %v", nvml.ErrorString(ret))
			// return nil, but it shouldn't be an error
			return nil, nil
		}

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
			FanSpeed:           uint(fanSpeed),
			PowerWatts:         uint(math.Round(float64(power) / 1000)),
			PowerLimitWatts:    uint(math.Round(float64(PowerLimitWatts) / 1000)),
			Processes:          processesInfo,
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
				log.Printf("Error updating GPU info: %v", err)
			} else {
				rl.mu.Lock()
				*rl.cache = gpuInfos
				rl.mu.Unlock()
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
			"/gpu/fan_speed":            func(gpuInfo *GPUInfo) interface{} { return gpuInfo.FanSpeed },
			"/gpu/all":                  func(gpuInfo *GPUInfo) interface{} { return gpuInfo },
			"/gpu/processes":            func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Processes },
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
