package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// totalPowerCap is GPU_TOTAL_POWER_CAP in watts, 0 when unset.
var totalPowerCap uint

// demandHeadroomW is how far below its current limit a GPU must draw before it
// counts as not needing that limit. NVML reports averaged draw, so a GPU held
// at its limit reads a little under it.
const demandHeadroomW = 30

// gpuBudget is one GPU's input to planPowerLimits, all in watts.
type gpuBudget struct {
	base    uint // limit without a total cap: the temperature tier, or the default limit
	min     uint // lowest limit the GPU accepts
	usage   uint // current draw
	current uint // currently enforced limit
}

// planPowerLimits returns a limit per GPU. Without a cap (totalCap 0), or when
// the base limits already fit under it, each GPU gets its base limit.
// Otherwise the limits are planned to sum to the cap: GPUs drawing near their
// limit share it evenly, lightly loaded GPUs keep their draw plus headroom, and
// whatever is left goes back towards the base limits.
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
		if g.usage+demandHeadroomW < g.current {
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

// readGPUBudget gathers the planning inputs for one GPU.
func readGPUBudget(device nvml.Device, index int) (gpuBudget, error) {
	var b gpuBudget

	usage, ret := device.GetPowerUsage()
	if ret != nvml.SUCCESS {
		return b, fmt.Errorf("unable to get power usage for GPU %d: %v", index, nvml.ErrorString(ret))
	}
	current, ret := device.GetPowerManagementLimit()
	if ret != nvml.SUCCESS {
		return b, fmt.Errorf("unable to get power limit for GPU %d: %v", index, nvml.ErrorString(ret))
	}
	minLimit, maxLimit, ret := device.GetPowerManagementLimitConstraints()
	if ret != nvml.SUCCESS {
		return b, fmt.Errorf("unable to get power limit constraints for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	if tiers, ok := gpuPowerLimits[index]; ok {
		temperature, ret := gpuTemperature(device)
		if ret != nvml.SUCCESS {
			return b, fmt.Errorf("unable to get temperature for GPU %d: %v", index, nvml.ErrorString(ret))
		}
		b.base = tempTierLimit(tiers, uint(temperature))
	} else {
		defaultLimit, ret := device.GetPowerManagementDefaultLimit()
		if ret != nvml.SUCCESS {
			return b, fmt.Errorf("unable to get default power limit for GPU %d: %v", index, nvml.ErrorString(ret))
		}
		b.base = uint(defaultLimit / 1000)
	}

	b.usage = uint(usage / 1000) // NVML reports milliwatts
	b.current = uint(current / 1000)
	b.min = uint(minLimit / 1000)
	b.base = min(b.base, uint(maxLimit/1000))
	return b, nil
}

func checkAndApplyPowerLimits() error {
	powerCheckMutex.Lock()
	defer powerCheckMutex.Unlock()

	if time.Since(lastTempCheckTime) < tempCheckInterval {
		return nil
	}

	lastTempCheckTime = time.Now()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
	}

	// GPUs without temperature tiers are only managed when a total cap can
	// apply, so they are restored to their default limit once it lifts.
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
		if ret != nvml.SUCCESS {
			return fmt.Errorf("unable to get device at index %d: %v", i, nvml.ErrorString(ret))
		}
		budget, err := readGPUBudget(device, i)
		if err != nil {
			return err
		}
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
