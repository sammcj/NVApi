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
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const version = "1.1.0"

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

type rateLimiter struct {
	tokens   float64
	capacity float64
	rate     float64
	mu       sync.Mutex
	lastTime time.Time
	cache    *[]GPUInfo // Add a cache variable to store cached GPUInfo data
}

type ProcessInfo struct {
	Pid             uint32
	UsedGpuMemoryMb uint64
	Name            string
	Arguments       []string
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
	// Return the cached response or a default value if no cache exists
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

func GetGPUInfo() ([]GPUInfo, error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to initialise NVML: %v", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()

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

		fanSpeed, ret := device.GetFanSpeed()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get fan speed: %v", nvml.ErrorString(ret))
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

var (
	port  = flag.Int("port", 9999, "Port to listen on")
	rate  = flag.Int("rate", 3, "Minimum number of seconds between requests")
	debug = flag.Bool("debug", false, "Print debug logs to the console")
	help  = flag.Bool("help", false, "Print this help")
)

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

	count, err := nvml.DeviceGetCount()
	if err != nvml.SUCCESS || count == 0 {
		log.Fatalf("no devices found")
	}

	if *help {
		flag.Usage()
		return
	}

	rl := &rateLimiter{
		capacity: float64(1),            // Use explicit conversion to avoid errors with integer division
		rate:     float64(*rate) / 3600, // Convert rate from seconds to hours for better readability
		cache:    new([]GPUInfo),        // Initialize cache as a pointer to slice of GPUInfo structs
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json") // Set Content-Type header for JSON response

		if !rl.takeToken() {
			// Rate limit exceeded, return the cached response or default value
			json.NewEncoder(w).Encode(rl.getCache())
			return
		}

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		*rl.cache = gpuInfos // Assign the returned GPUInfo slice to cache
		json.NewEncoder(w).Encode(gpuInfos)

		if *debug {
			fmt.Println("GPU Info: ", gpuInfos)
		}
	})

	http.HandleFunc("/gpu", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json") // Set Content-Type header for JSON response

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

		// if debug is enabled, print the path
		if *debug {
			fmt.Println("Path: ", r.URL.Path)
		}

		// if debug is enable print the request
		if *debug {
			fmt.Println("Request: ", r)
		}

		if !rl.takeToken() {
			// Rate limit exceeded, return the cached response or default value
			json.NewEncoder(w).Encode(rl.getCache())
			return
		}

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		*rl.cache = gpuInfos // Assign the returned GPUInfo slice to cache

		for _, gpuInfo := range gpuInfos {
			path := r.URL.Path
			if f, ok := pathToField[path]; ok {
				json.NewEncoder(w).Encode(f(&gpuInfo))
			} else {
				json.NewEncoder(w).Encode(gpuInfo)
			}
		}
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))

	if err := nvml.Shutdown(); err != nvml.SUCCESS {
		log.Fatalf("unable to shutdown NVML: %v", nvml.ErrorString(err))
	}
}
