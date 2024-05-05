package main

// This is a basic API that returns Nvidia GPU utilisation information.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const version = "1.0.0"

type GPUInfo struct {
	GPUUtilization  uint `json:"gpu_utilization"`
	MemoryUtilization uint `json:"memory_utilization"`
	PowerWatts          uint `json:"power_watts"`
	MemoryTotal    float64 `json:"memory_total_gb"`
	MemoryUsed    float64 `json:"memory_used_gb"`
	MemoryFree    float64 `json:"memory_free_gb"`
	MemoryUsage   string `json:"memory_usage"`
	Temperature   uint `json:"temperature"`
	FanSpeed      uint `json:"fan_speed"`
}

type rateLimiter struct {
	tokens  float64
	capacity float64
	rate     float64
	mu       sync.Mutex
	lastTime time.Time
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

func GetGPUInfo() (*GPUInfo, error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to initialize NVML: %v", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	if count == 0 {
		return nil, fmt.Errorf("no devices found")
	}

	device, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get device at index 0: %v", nvml.ErrorString(ret))
	}

	usage, ret := device.GetUtilizationRates()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get utilization rates: %v", nvml.ErrorString(ret))
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

	memoryTotal := float64(memory.Total) / 1024 / 1024 / 1024
	memoryUsed := float64(memory.Used) / 1024 / 1024 / 1024
	memoryFree := float64(memory.Free) / 1024 / 1024 / 1024
	memoryUsage := fmt.Sprintf("%d%%", int(math.Round((float64(memory.Used)/float64(memory.Total))*100)))

	return &GPUInfo{
		GPUUtilization:  uint(usage.Gpu),
		MemoryUtilization: uint(usage.Memory),
		PowerWatts:          uint(math.Round(float64(power) / 1000)), // Convert milliwatts to watts and round to whole number
		MemoryTotal:    math.Round(memoryTotal*100) / 100, // Round to 2 decimal places
		MemoryUsed:    math.Round(memoryUsed*100) / 100, // Round to 2 decimal places
		MemoryFree:    math.Round(memoryFree*100) / 100, // Round to 2 decimal places
		MemoryUsage:   memoryUsage,
		Temperature:   uint(temperature),
		FanSpeed:      uint(fanSpeed),
	}, nil
}

var (
	port     = flag.Int("port", 9999, "Port to listen on")
	rate     = flag.Int("rate", 1, "Minimum number of seconds between requests")
	help 		 = flag.Bool("help", false, "Print this help")
	lastGPUInfo *GPUInfo
)

func main() {
	println("NVApi Version: ", version)
	flag.Parse()

	// print help if an invalid argument is passed or -h --help is passed
	if *port < 1 || *rate < 1 {
		flag.Usage()
		return
	}

	if err := nvml.Init(); err != nvml.SUCCESS {
		log.Fatalf("unable to initialize NVML: %v", nvml.ErrorString(err))
	}

	if count, err := nvml.DeviceGetCount(); err != nvml.SUCCESS || count == 0 {
		log.Fatalf("no devices found")
	}

	// if arg is -h or --help print help
	if *help {
		flag.Usage()
		return
	}

	rl := &rateLimiter{
		capacity: 1,
		rate:     1 / float64(*rate), // Convert seconds to rate
	}

	http.HandleFunc("/gpu", func(w http.ResponseWriter, r *http.Request) {
		if !rl.takeToken() {
			if lastGPUInfo != nil {
				json.NewEncoder(w).Encode(lastGPUInfo)
			} else {
				http.Error(w, "No data available", http.StatusNoContent)
			}
			return
		}

		info, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		lastGPUInfo = info
		json.NewEncoder(w).Encode(info)
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))


	if err := nvml.Shutdown(); err != nvml.SUCCESS {
		log.Fatalf("unable to shutdown NVML: %v", nvml.ErrorString(err))
	}
}
