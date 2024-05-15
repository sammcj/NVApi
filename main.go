package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const version = "1.0.1"

type GPUInfo struct {
	GPUUtilisation  uint `json:"gpu_utilisation"`
	MemoryUtilisation uint `json:"memory_utilisation"`
	PowerWatts          uint `json:"power_watts"`
	MemoryTotal    float64 `json:"memory_total_gb"`
	MemoryUsed    float64 `json:"memory_used_gb"`
	MemoryFree    float64 `json:"memory_free_gb"`
	MemoryUsage   string `json:"memory_usage"`
	Temperature   uint `json:"temperature"`
	FanSpeed      uint `json:"fan_speed"`
	Processes		 []ProcessInfo `json:"processes"`
}

type rateLimiter struct {
	tokens  float64
	capacity float64
	rate     float64
	mu       sync.Mutex
	lastTime time.Time
}

type ProcessInfo struct {
	Pid                uint32
	UsedGpuMemoryMb      uint64
	Name              string
	Arguments				 []string
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
		device, ret := nvml.DeviceGetHandleByIndex(int(i))
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
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
		for i, process := range processes {
			// now use the pid to get the process name (e.g. /bin/ollama serve)
			cmd := exec.Command("ps", "-p", fmt.Sprintf("%d", process.Pid), "-o", "command=")
			output, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("unable to get process name: %v", err)
			}
			// remove anything after the first newline
			output = []byte(strings.Split(string(output), "\n")[0])

			// split the output process string into a slice of strings, the first element is the process name, the rest are the arguments
			processString := strings.Split(string(output), " ")
			processName := processString[0]
			arguments := processString[1:]
			// if arguments are longer than 500 characters, truncate them
			if len(arguments) > 500 {
				arguments = arguments[:500]
			}

			// append the new process info struct to the processesInfo slice
			processesInfo[i] = ProcessInfo{
				Pid: process.Pid,
				UsedGpuMemoryMb: process.UsedGpuMemory / 1024 / 1024,
				Name: processName,
				Arguments: arguments,
			}

			if debug != nil && *debug {
				fmt.Println("Process: ", processName)
			}
		}

		if debug != nil && *debug {
			fmt.Println("Processes: ", processesInfo)
		}

		memoryTotal := float64(memory.Total) / 1024 / 1024 / 1024
		memoryUsed := float64(memory.Used) / 1024 / 1024 / 1024
		memoryFree := float64(memory.Free) / 1024 / 1024 / 1024
		memoryUsage := fmt.Sprintf("%d", int(math.Round((float64(memory.Used)/float64(memory.Total))*100)))

		gpuInfo := GPUInfo{
			GPUUtilisation:  uint(usage.Gpu),
			MemoryUtilisation: uint(usage.Memory),
			PowerWatts:          uint(math.Round(float64(power) / 1000)),
			MemoryTotal:    math.Round(memoryTotal*100) / 100,
			MemoryUsed:    math.Round(memoryUsed*100) / 100,
			MemoryFree:    math.Round(memoryFree*100) / 100,
			MemoryUsage:   memoryUsage,
			Temperature:   uint(temperature),
			FanSpeed:      uint(fanSpeed),
			Processes:		 processesInfo,
		}

		gpuInfos[i] = gpuInfo
	}

	return gpuInfos, nil
}

var (
	port     = flag.Int("port", 9999, "Port to listen on")
	rate     = flag.Int("rate", 3, "Minimum number of seconds between requests")
	debug	 	= flag.Bool("debug", false, "Print debug logs to the console")
	help 		 = flag.Bool("help", false, "Print this help")
	lastGPUInfos []*GPUInfo
)

func generateComputeSVG(gpuInfo GPUInfo) string {
	width := 200
	height := 20

	gpuUtilisation := gpuInfo.GPUUtilisation

	svg := fmt.Sprintf(`
				<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d">
            <rect x="0" y="0" width="%d" height="%d" fill="#CCCCCC" rx="5" />
            <rect x="0" y="0" width="%d" height="%d" fill="#00FF00" rx="5" />
            <rect x="%d" y="0" width="%d" height="%d" fill="#FFFF00" rx="5" />
            <text x="5" y="15" font-family="Arial" font-size="10" fill="black">GPU: %d%%</text>
        </svg>`,
		width, height,
		width, height,
		int(float64(width)*float64(gpuUtilisation)/100), height,
		width + int(float64(width)*float64(gpuUtilisation)/100),
		int(float64(width)*float64(gpuUtilisation)/100), height,
		gpuUtilisation)

	return svg
}

func generateMemSVG(gpuInfo GPUInfo) string {
	width := 200
	height := 20

	memoryUtilisation := gpuInfo.MemoryUtilisation

	svg := fmt.Sprintf(`
				<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d">
						<rect x="0" y="0" width="%d" height="%d" fill="#CCCCCC" rx="5" />
						<rect x="0" y="0" width="%d" height="%d" fill="#00FF00" rx="5" />
						<rect x="%d" y="0" width="%d" height="%d" fill="#FFFF00" rx="5" />
						<text x="5" y="15" font-family="Arial" font-size="10" fill="black">Memory: %d%%</text>
				</svg>`,
		width, height,
		width, height,
		int(float64(width)*float64(memoryUtilisation)/100), height,
		width + int(float64(width)*float64(memoryUtilisation)/100),
		int(float64(width)*float64(memoryUtilisation)/100), height,
		memoryUtilisation)

	return svg
}

func main() {
	println("NVApi Version: ", version)
	flag.Parse()

	if debug != nil && *debug {
		println("*** Debug Mode ***")
		println("Port: ", *port)
		println("Rate: ", *rate)
		println("***            ***")
	}

	if *port < 1 || *rate < 1 {
		flag.Usage()
		return
	}

	if err := nvml.Init(); err != nvml.SUCCESS {
		log.Fatalf("unable to initialise NVML: %v", nvml.ErrorString(err))
	}

	if count, err := nvml.DeviceGetCount(); err != nvml.SUCCESS || count == 0 {
		log.Fatalf("no devices found")
	}

	if *help {
		flag.Usage()
		return
	}

	rl := &rateLimiter{
		capacity: 1,
		rate:     1 / float64(*rate),
	}

	http.HandleFunc("/svg/compute", func(w http.ResponseWriter, r *http.Request) {
		if !rl.takeToken() {
			if lastGPUInfos != nil {
				w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
				for _, gpuInfo := range lastGPUInfos {
					svg := generateComputeSVG(*gpuInfo)
					fmt.Fprint(w, svg)
				}
			} else {
				http.Error(w, "No data available", http.StatusNoContent)
			}
			return
		}

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		lastGPUInfos = make([]*GPUInfo, len(gpuInfos))
		for i, gpuInfo := range gpuInfos {
			lastGPUInfos[i] = &gpuInfo
		}

		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		for _, gpuInfo := range lastGPUInfos {
			svg := generateComputeSVG(*gpuInfo)
			fmt.Fprint(w, svg)
		}
	})

	http.HandleFunc("/svg/mem", func(w http.ResponseWriter, r *http.Request) {
		if !rl.takeToken() {
			if lastGPUInfos != nil {
				w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
				for _, gpuInfo := range lastGPUInfos {
					svg := generateMemSVG(*gpuInfo)
					fmt.Fprint(w, svg)
				}
			} else {
				http.Error(w, "No data available", http.StatusNoContent)
			}
			return
		}

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		lastGPUInfos = make([]*GPUInfo, len(gpuInfos))
		for i, gpuInfo := range gpuInfos {
			lastGPUInfos[i] = &gpuInfo
		}

		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		for _, gpuInfo := range lastGPUInfos {
			svg := generateMemSVG(*gpuInfo)
			fmt.Fprint(w, svg)
		}
	})

	// // Add a handler for the / endpoint that returns the /gpu endpoint
	// http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	// 	http.Redirect(w, r, "/gpu", http.StatusSeeOther)
	// })

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// if /gpu is requested, return the full GPU info

		// if the rate limiter does not allow, return the last GPU info
		if !rl.takeToken() {
			if lastGPUInfos != nil {
				json.NewEncoder(w).Encode(lastGPUInfos)
			} else {
				http.Error(w, "No data available", http.StatusNoContent)
			}
			return
		}

		// if the rate limiter allows, get the GPU info

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		lastGPUInfos = make([]*GPUInfo, len(gpuInfos))
		for i, gpuInfo := range gpuInfos {
			lastGPUInfos[i] = &gpuInfo
		}

		json.NewEncoder(w).Encode(gpuInfos)
	})

	http.HandleFunc("/gpu", func(w http.ResponseWriter, r *http.Request) {
		pathToField := map[string]func(gpuInfo *GPUInfo) interface{}{
			"/gpu/gpu_utilisation": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.GPUUtilisation },
			"/gpu/memory_utilisation": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUtilisation },
			"/gpu/power_watts": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.PowerWatts },
			"/gpu/memory_total_gb": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryTotal },
			"/gpu/memory_used_gb": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUsed },
			"/gpu/memory_free_gb": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryFree },
			"/gpu/memory_usage": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.MemoryUsage },
			"/gpu/temperature": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Temperature },
			"/gpu/fan_speed": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.FanSpeed },
			"/gpu/processes": func(gpuInfo *GPUInfo) interface{} { return gpuInfo.Processes },
			"/gpu/all": func(gpuInfo *GPUInfo) interface{} { return gpuInfo },
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
			if lastGPUInfos != nil {
				for _, gpuInfo := range lastGPUInfos {
					path := r.URL.Path
					if f, ok := pathToField[path]; ok {
						json.NewEncoder(w).Encode(f(gpuInfo))
						if debug != nil && *debug {
							fmt.Println("Field: ", f(gpuInfo))
						}
					} else {
						json.NewEncoder(w).Encode(gpuInfo)
						if debug != nil && *debug {
							fmt.Println("GPU Info: ", gpuInfo)
						}
					}
				}
			} else {
				http.Error(w, "No data available", http.StatusNoContent)
			}
			return
		}

		gpuInfos, err := GetGPUInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, gpuInfo := range gpuInfos {
			path := r.URL.Path
			if f, ok := pathToField[path]; ok {
				json.NewEncoder(w).Encode(f(&gpuInfo))
			} else {
				json.NewEncoder(w).Encode(gpuInfo)
			}
		}
	})

	http.HandleFunc("/svg", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/svg/mem", http.StatusSeeOther)
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))

	if err := nvml.Shutdown(); err != nvml.SUCCESS {
		log.Fatalf("unable to shutdown NVML: %v", nvml.ErrorString(err))
	}
}
