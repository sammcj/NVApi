package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

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
}



func NewPCIeStateManager(devices []nvml.Device) *PCIeStateManager {
	manager := &PCIeStateManager{
		devices:            devices,
		utilisationHistory: make(map[int][]float64),
		lastChangeTime:     make(map[int]time.Time),
		configs:            make(map[int]*PCIeConfig),
	}

	manager.loadConfigurations()
	return manager
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

		m.utilisationHistory[i] = append(m.utilisationHistory[i], float64(usage.Gpu))
		if len(m.utilisationHistory[i]) > m.configs[i].IdleThresholdSeconds {
			m.utilisationHistory[i] = m.utilisationHistory[i][1:]
		}

		if m.shouldChangeLinkSpeed(i) {
			m.changeLinkSpeed(i, device)
		}
	}
}

func (m *PCIeStateManager) shouldChangeLinkSpeed(index int) bool {
	history := m.utilisationHistory[index]
	config := m.configs[index]

	if len(history) < config.IdleThresholdSeconds {
		return false
	}

	for _, util := range history {
		if util >= 1.0 {
			return false
		}
	}

	lastChange, exists := m.lastChangeTime[index]
	if !exists || time.Since(lastChange) >= 5*time.Minute {
		return true
	}

	return false
}

func (m *PCIeStateManager) changeLinkSpeed(index int, device nvml.Device) {
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		log.Printf("Failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
		return
	}

	deviceAddress := fmt.Sprintf("%04x:%02x:%02x.0", pciInfo.Domain, pciInfo.Bus, pciInfo.Device)

	err := setPCIeSpeed(deviceAddress, m.configs[index].MinSpeed)
	if err != nil {
		log.Printf("Failed to set PCIe speed for GPU %d: %v", index, err)
		return
	}

	log.Printf("Set PCIe link speed to %d for GPU %d", m.configs[index].MinSpeed, index)
	m.lastChangeTime[index] = time.Now()
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
