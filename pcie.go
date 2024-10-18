package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
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

func (m *PCIeStateManager) loadConfigurations() {
	for i := range m.devices {
		m.configs[i] = &PCIeConfig{
			IdleThresholdSeconds: getEnvAsInt(fmt.Sprintf("GPU_%d_PCIE_IDLE_THRESHOLD", i), 20),
			Enabled:              getEnvAsBool(fmt.Sprintf("GPU_%d_PCIE_MANAGEMENT_ENABLED", i), false),
		}
	}

	// Global configuration (applied to all GPUs if not set individually)
	globalThreshold := getEnvAsInt("GPU_PCIE_IDLE_THRESHOLD", 20)
	globalEnabled := getEnvAsBool("GPU_PCIE_MANAGEMENT_ENABLED", false)

	for i, config := range m.configs {
		if !config.Enabled {
			config.Enabled = globalEnabled
		}
		if config.IdleThresholdSeconds == 20 { // If not set individually
			config.IdleThresholdSeconds = globalThreshold
		}
		log.Printf("GPU %d PCIe management: enabled=%v, idle threshold=%d seconds",
			i, config.Enabled, config.IdleThresholdSeconds)
	}
}

func (m *PCIeStateManager) UpdateUtilization() {
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

		if m.shouldChangeLinkState(i) {
			m.changeLinkState(i, device)
		}
	}
}

func (m *PCIeStateManager) shouldChangeLinkState(index int) bool {
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

func (m *PCIeStateManager) changeLinkState(index int, device nvml.Device) {
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		log.Printf("Failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
		return
	}

	domainBus := fmt.Sprintf("%04x:%02x:%02x.0", pciInfo.Domain, pciInfo.Bus, pciInfo.Device)
	powerControlFile := filepath.Join(pcieSysfsPath, domainBus, "power", "control")

	// Check if power control is available
	if _, err := os.Stat(powerControlFile); os.IsNotExist(err) {
		log.Printf("Power control not available for GPU %d", index)
		return
	}

	// Set power control to "auto" to enable power saving
	err := ioutil.WriteFile(powerControlFile, []byte("auto"), 0644)
	if err != nil {
		log.Printf("Failed to set power control for GPU %d: %v", index, err)
		return
	}

	log.Printf("Set PCIe link state to power saving mode for GPU %d", index)
	m.lastChangeTime[index] = time.Now()
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
