package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// totalPowerCap is GPU_TOTAL_POWER_CAP in watts, 0 when unset.
var totalPowerCap uint

// demandHeadroomW is the headroom above current draw kept by a GPU that isn't
// power limited, when a total cap squeezes it.
const demandHeadroomW = 30

// busyHold keeps a GPU classed as busy for a while after it was last power
// limited. Inference load is bursty, so a single sample can catch it between
// bursts with a low averaged draw.
const busyHold = 30 * time.Second

// State owned by checkAndApplyPowerLimits (under powerCheckMutex).
var (
	startupPowerLimits  map[int]uint          // limits of GPUs without tiers when NVApi started
	lastPowerLimited    = map[int]time.Time{} // when each GPU was last seen power limited
	gpuBudgetErrors     = map[int]string{}    // last read error logged per GPU
	powerCheckRequested atomic.Bool           // skip the interval gate on the next check
)

// gpuBudget is one GPU's input to planPowerLimits, all in watts.
type gpuBudget struct {
	base    uint // limit without a total cap: the temperature tier, or the startup limit
	min     uint // lowest limit the GPU accepts
	usage   uint // current draw
	current uint // currently enforced limit
	busy    bool // power limited recently, so it needs its full share
}

// powerLimited reports whether a GPU is being held back by its power limit,
// from NVML's throttle reason or from drawing close to the limit.
//
// The draw band is half the headroom a squeezed light GPU is granted, so a
// light GPU still drawing what it did isn't reclassified as busy next cycle.
func powerLimited(throttled bool, usage, current uint) bool {
	return throttled || usage+demandHeadroomW/2 >= current
}

// planPowerLimits returns a limit per GPU. Without a cap (totalCap 0), or when
// the base limits already fit under it, each GPU gets its base limit.
// Otherwise the limits are planned to sum to the cap: busy GPUs share it
// evenly, lightly loaded GPUs keep their draw plus headroom, and whatever is
// left goes back towards the base limits.
//
// Capping the sum of limits, rather than reacting once draw exceeds the cap,
// means a GPU ramping up can't overshoot the cap. Planning from the base limit
// every cycle restores limits as soon as the cap allows.
func planPowerLimits(gpus []gpuBudget, totalCap uint) []uint {
	bases := make([]uint, len(gpus))
	var baseSum, minSum uint
	for i, g := range gpus {
		bases[i] = max(g.base, g.min)
		baseSum += bases[i]
		minSum += g.min
	}
	if totalCap == 0 || baseSum <= totalCap {
		return bases
	}

	limits := make([]uint, len(gpus))
	demand := make([]uint, len(gpus))
	for i, g := range gpus {
		limits[i] = g.min
		demand[i] = bases[i]
		if !g.busy {
			demand[i] = max(g.min, min(bases[i], g.usage+demandHeadroomW))
		}
	}
	if minSum >= totalCap {
		return limits // the hardware minimums already exceed the cap
	}

	spare := fillTo(limits, demand, totalCap-minSum)
	fillTo(limits, bases, spare)
	return limits
}

// fillTo raises limits towards ceilings in equal steps until the budget runs
// out, so a constrained budget is shared evenly. It returns the unspent budget.
func fillTo(limits, ceilings []uint, budget uint) uint {
	for budget > 0 {
		open := 0
		for i := range limits {
			if limits[i] < ceilings[i] {
				open++
			}
		}
		if open == 0 {
			break
		}
		share := max(budget/uint(open), 1)
		for i := range limits {
			if budget == 0 {
				break
			}
			if limits[i] >= ceilings[i] {
				continue
			}
			step := min(share, ceilings[i]-limits[i], budget)
			limits[i] += step
			budget -= step
		}
	}
	return budget
}

// tempTierLimit picks the configured limit for the GPU's current temperature.
func tempTierLimit(limits TempPowerLimits, temp uint) uint {
	switch {
	case temp <= uint(limits.LowTemp):
		return limits.LowTempLimit
	case temp <= uint(limits.MediumTemp):
		return limits.MediumTempLimit
	default:
		return limits.HighTempLimit
	}
}

// effectiveTotalCap is the tighter of GPU_TOTAL_POWER_CAP and an active UPS guard cap.
func effectiveTotalCap() uint {
	limit := totalPowerCap
	if ups := upsGuardInstance.activeCap(); ups > 0 && (limit == 0 || ups < limit) {
		limit = ups
	}
	return limit
}

func parseTotalPowerCap() {
	capStr := os.Getenv("GPU_TOTAL_POWER_CAP")
	if capStr == "" {
		totalPowerCap = 0 // Disabled if not set
		return
	}

	capValue, err := strconv.ParseUint(capStr, 10, 32)
	if err != nil {
		log.Printf("Warning: Invalid GPU_TOTAL_POWER_CAP value. Total power cap will be disabled.")
		totalPowerCap = 0
		return
	}

	totalPowerCap = uint(capValue)
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

// go-nvml links NVML symbols lazily, so calling nvmlDeviceGetTemperatureV on a
// driver that predates it would crash rather than return an error.
var hasTemperatureV = sync.OnceValue(func() bool {
	return nvml.Extensions().LookupSymbol("nvmlDeviceGetTemperatureV") == nil
})

func gpuTemperature(device nvml.Device) (uint32, nvml.Return) {
	if !hasTemperatureV() {
		return device.GetTemperature(nvml.TEMPERATURE_GPU) //nolint:staticcheck // fallback for older drivers
	}
	t, ret := device.GetTemperatureV().V1()
	return uint32(t.Temperature), ret
}

// sumDefaultPowerLimits returns the GPUs' summed default power limits in watts,
// skipping any GPU that can't report one.
func sumDefaultPowerLimits(count int) uint {
	var total uint
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		if limit, ret := device.GetPowerManagementDefaultLimit(); ret == nvml.SUCCESS {
			total += uint(limit / 1000)
		}
	}
	return total
}

// snapshotStartupPowerLimits records the current limit of each GPU without
// temperature tiers. Those GPUs are only lowered to fit a total cap, and this
// is the limit they return to, so limits set outside NVApi are kept.
func snapshotStartupPowerLimits(count int) {
	startupPowerLimits = make(map[int]uint)
	for i := 0; i < count; i++ {
		if _, hasTiers := gpuPowerLimits[i]; hasTiers {
			continue
		}
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		if limit, ret := device.GetPowerManagementLimit(); ret == nvml.SUCCESS {
			startupPowerLimits[i] = uint(limit / 1000)
		}
	}
}

// readGPUBudget gathers the planning inputs for one GPU. Only the current limit
// (and temperature, for tiered GPUs) are required; GPUs that can't report the
// rest are planned conservatively.
func readGPUBudget(device nvml.Device, index int, now time.Time) (gpuBudget, error) {
	var b gpuBudget

	current, ret := device.GetPowerManagementLimit()
	if ret != nvml.SUCCESS {
		return b, fmt.Errorf("unable to get power limit: %v", nvml.ErrorString(ret))
	}
	b.current = uint(current / 1000) // NVML reports milliwatts

	if tiers, ok := gpuPowerLimits[index]; ok {
		temperature, ret := gpuTemperature(device)
		if ret != nvml.SUCCESS {
			return b, fmt.Errorf("unable to get temperature: %v", nvml.ErrorString(ret))
		}
		b.base = tempTierLimit(tiers, uint(temperature))
	} else if limit, ok := startupPowerLimits[index]; ok {
		b.base = limit
	} else {
		return b, fmt.Errorf("no startup power limit recorded")
	}

	if minLimit, maxLimit, ret := device.GetPowerManagementLimitConstraints(); ret == nvml.SUCCESS {
		b.min = uint(minLimit / 1000)
		b.base = min(b.base, uint(maxLimit/1000))
	}

	usage, ret := device.GetPowerUsage()
	if ret != nvml.SUCCESS {
		b.busy = true // without a reading, assume it needs its full share
		return b, nil
	}
	b.usage = uint(usage / 1000)

	reasons, ret := device.GetCurrentClocksEventReasons()
	throttled := ret == nvml.SUCCESS && reasons&nvml.ClocksEventReasonSwPowerCap != 0
	if powerLimited(throttled, b.usage, b.current) {
		lastPowerLimited[index] = now
	}
	if last, ok := lastPowerLimited[index]; ok && now.Sub(last) < busyHold {
		b.busy = true
	}
	return b, nil
}

// requestPowerCheck runs a power limit check now rather than at the next interval.
func requestPowerCheck() {
	powerCheckRequested.Store(true)
	if err := checkAndApplyPowerLimits(); err != nil {
		log.Printf("Warning: Failed to check and apply power limits: %v", err)
	}
}

func checkAndApplyPowerLimits() error {
	powerCheckMutex.Lock()
	defer powerCheckMutex.Unlock()

	if !powerCheckRequested.Swap(false) && time.Since(lastTempCheckTime) < tempCheckInterval {
		return nil
	}

	now := time.Now()
	lastTempCheckTime = now

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	// GPUs without temperature tiers are only managed when a total cap can apply.
	capConfigured := totalPowerCap > 0 || upsGuardInstance != nil
	totalCap := effectiveTotalCap()

	var indices []int
	var devices []nvml.Device
	var budgets []gpuBudget
	for i := 0; i < int(count); i++ {
		if _, hasTiers := gpuPowerLimits[i]; !hasTiers && !capConfigured {
			continue
		}
		device, ret := nvml.DeviceGetHandleByIndex(i)
		var budget gpuBudget
		var err error
		if ret != nvml.SUCCESS {
			err = fmt.Errorf("unable to get device: %v", nvml.ErrorString(ret))
		} else {
			budget, err = readGPUBudget(device, i, now)
		}
		// One GPU failing (e.g. a display card without power management)
		// mustn't stop the others being managed, so skip it and log once.
		if err != nil {
			if gpuBudgetErrors[i] != err.Error() {
				log.Printf("Warning: Not managing power limit of GPU %d: %v", i, err)
				gpuBudgetErrors[i] = err.Error()
			}
			continue
		}
		delete(gpuBudgetErrors, i)
		indices = append(indices, i)
		devices = append(devices, device)
		budgets = append(budgets, budget)
	}

	for k, limit := range planPowerLimits(budgets, totalCap) {
		b := budgets[k]
		// Re-setting the same value every cycle is a redundant control-path
		// write that wakes idle GPUs (and defeats runtime power management).
		if limit == b.current {
			continue
		}
		if ret := devices[k].SetPowerManagementLimit(uint32(limit * 1000)); ret != nvml.SUCCESS {
			log.Printf("Warning: Failed to set power limit for GPU %d to %d W: %v", indices[k], limit, nvml.ErrorString(ret))
			continue
		}
		if limit < b.base {
			log.Printf("GPU %d power limit set to %d W to fit total cap of %d W (drawing %d W, uncapped limit %d W)",
				indices[k], limit, totalCap, b.usage, b.base)
		} else if *debug {
			log.Printf("GPU %d power limit set to %d W", indices[k], limit)
		}
	}

	return nil
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
