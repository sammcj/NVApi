package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Defaults used when a threshold or cap isn't set explicitly.
const (
	defaultUPSHighFraction   = 0.9 // of ups.realpower.nominal
	defaultUPSLowFraction    = 0.5 // of ups.realpower.nominal
	defaultUPSGPUCapFraction = 0.5 // of the GPUs' summed default power limits
)

// upsGuardInstance is nil unless UPS_NUT_HOST is set.
var upsGuardInstance *upsGuard

type upsGuardConfig struct {
	nut          nutClient
	upsName      string // empty: the first UPS upsd lists
	pollInterval time.Duration
	highW        uint // cap GPUs at or above this UPS draw; 0 derives it from the UPS rating
	lowW         uint // lift the cap after cooldown below this; 0 derives it from the UPS rating
	cooldown     time.Duration
	gpuCapW      uint // total GPU power limit while capped; 0 derives it from the GPU defaults
}

// parseUPSGuardConfig reads the UPS_* settings. ok is false when UPS_NUT_HOST is unset.
func parseUPSGuardConfig(getenv func(string) string) (cfg upsGuardConfig, ok bool, err error) {
	host := strings.TrimSpace(getenv("UPS_NUT_HOST"))
	if host == "" {
		return cfg, false, nil
	}

	port, err := envUint(getenv, "UPS_NUT_PORT", 3493)
	if err != nil {
		return cfg, false, err
	}
	poll, err := envUint(getenv, "UPS_POLL_INTERVAL", 5)
	if err != nil {
		return cfg, false, err
	}
	if poll == 0 {
		return cfg, false, errors.New("UPS_POLL_INTERVAL must be at least 1 second")
	}
	cooldown, err := envUint(getenv, "UPS_COOLDOWN", 600)
	if err != nil {
		return cfg, false, err
	}
	if cfg.highW, err = envUint(getenv, "UPS_POWER_HIGH", 0); err != nil {
		return cfg, false, err
	}
	if cfg.lowW, err = envUint(getenv, "UPS_POWER_LOW", 0); err != nil {
		return cfg, false, err
	}
	if cfg.highW > 0 && cfg.lowW >= cfg.highW {
		return cfg, false, fmt.Errorf("UPS_POWER_LOW (%d W) must be below UPS_POWER_HIGH (%d W)", cfg.lowW, cfg.highW)
	}
	if cfg.gpuCapW, err = envUint(getenv, "UPS_GPU_POWER_CAP", 0); err != nil {
		return cfg, false, err
	}

	cfg.nut = nutClient{
		addr:     net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)),
		username: getenv("UPS_NUT_USERNAME"),
		password: getenv("UPS_NUT_PASSWORD"),
		timeout:  5 * time.Second,
	}
	cfg.upsName = strings.TrimSpace(getenv("UPS_NUT_NAME"))
	cfg.pollInterval = time.Duration(poll) * time.Second
	cfg.cooldown = time.Duration(cooldown) * time.Second
	return cfg, true, nil
}

func envUint(getenv func(string) string, name string, def uint) (uint, error) {
	s := strings.TrimSpace(getenv(name))
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be a whole number", name, s)
	}
	return uint(v), nil
}

// upsGuard caps total GPU power while the UPS is heavily loaded, and lifts the
// cap once the UPS has stayed below the low threshold for the cooldown period.
type upsGuard struct {
	cfg  upsGuardConfig
	capW atomic.Uint32 // GPU cap while active, 0 otherwise; read by the power limit loop

	// Owned by the poll goroutine.
	active   bool
	lowSince time.Time
	lastErr  string
}

// newUPSGuard resolves the GPU cap, defaulting to a fraction of the GPUs'
// summed default limits (gpuDefaultsW).
func newUPSGuard(cfg upsGuardConfig, gpuDefaultsW uint) (*upsGuard, error) {
	if cfg.gpuCapW == 0 {
		cfg.gpuCapW = uint(float64(gpuDefaultsW) * defaultUPSGPUCapFraction)
	}
	if cfg.gpuCapW == 0 {
		return nil, errors.New("UPS_GPU_POWER_CAP is not set and the GPU default power limits are unknown")
	}
	return &upsGuard{cfg: cfg}, nil
}

// activeCap returns the GPU cap in watts while the guard is tripped, otherwise 0.
func (g *upsGuard) activeCap() uint {
	if g == nil {
		return 0
	}
	return uint(g.capW.Load())
}

// observe applies one UPS power reading.
func (g *upsGuard) observe(powerW, highW, lowW uint, now time.Time) {
	if !g.active {
		if powerW >= highW {
			g.active = true
			g.lowSince = time.Time{}
			g.capW.Store(uint32(g.cfg.gpuCapW))
			log.Printf("UPS guard: UPS drawing %d W (limit %d W), capping GPUs at %d W total", powerW, highW, g.cfg.gpuCapW)
		}
		return
	}
	if powerW >= lowW {
		g.lowSince = time.Time{}
		return
	}
	if g.lowSince.IsZero() {
		g.lowSince = now
	}
	if now.Sub(g.lowSince) >= g.cfg.cooldown {
		g.active = false
		g.capW.Store(0)
		log.Printf("UPS guard: UPS below %d W for %s, removing GPU cap", lowW, g.cfg.cooldown)
	}
}

// poll reads the UPS once and updates the guard.
func (g *upsGuard) poll(now time.Time) error {
	if g.cfg.upsName == "" {
		names, err := g.cfg.nut.listUPS()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("upsd at %s serves no UPS", g.cfg.nut.addr)
		}
		g.cfg.upsName = names[0]
		log.Printf("UPS guard: UPS_NUT_NAME not set, using %q", g.cfg.upsName)
	}

	vars, err := g.cfg.nut.getVars(g.cfg.upsName, "ups.realpower", "ups.load", "ups.realpower.nominal")
	if err != nil {
		return err
	}
	powerW, err := upsPowerWatts(vars)
	if err != nil {
		return err
	}
	highW, lowW, err := g.thresholds(vars)
	if err != nil {
		return err
	}
	g.observe(powerW, highW, lowW, now)
	return nil
}

// thresholds returns the configured thresholds, deriving unset ones from the UPS rating.
func (g *upsGuard) thresholds(vars map[string]string) (highW, lowW uint, err error) {
	highW, lowW = g.cfg.highW, g.cfg.lowW
	if highW == 0 || lowW == 0 {
		nominal, err := parseNUTNumber(vars, "ups.realpower.nominal")
		if err != nil || nominal <= 0 {
			return 0, 0, errors.New("UPS doesn't report ups.realpower.nominal: set UPS_POWER_HIGH and UPS_POWER_LOW")
		}
		if highW == 0 {
			highW = uint(nominal * defaultUPSHighFraction)
		}
		if lowW == 0 {
			lowW = uint(nominal * defaultUPSLowFraction)
		}
	}
	if lowW >= highW {
		return 0, 0, fmt.Errorf("UPS low threshold (%d W) must be below the high threshold (%d W)", lowW, highW)
	}
	return highW, lowW, nil
}

// run polls the UPS until the process exits.
func (g *upsGuard) run() {
	ticker := time.NewTicker(g.cfg.pollInterval)
	defer ticker.Stop()
	for {
		g.pollOnce(time.Now())
		<-ticker.C
	}
}

func (g *upsGuard) pollOnce(now time.Time) {
	err := g.poll(now)
	switch {
	case err != nil:
		// Without a reading the load can't be confirmed low, so restart the cooldown.
		g.lowSince = time.Time{}
		if err.Error() != g.lastErr {
			log.Printf("Warning: UPS guard: %v (keeping current GPU cap state)", err)
			g.lastErr = err.Error()
		}
	case g.lastErr != "":
		log.Printf("UPS guard: reading UPS %q again", g.cfg.upsName)
		g.lastErr = ""
	}
}

// upsPowerWatts returns the UPS output in watts, estimating it from the load
// percentage when the UPS doesn't report real power.
func upsPowerWatts(vars map[string]string) (uint, error) {
	if _, ok := vars["ups.realpower"]; ok {
		w, err := parseNUTNumber(vars, "ups.realpower")
		if err != nil {
			return 0, err
		}
		return uint(math.Round(max(w, 0))), nil
	}
	load, loadErr := parseNUTNumber(vars, "ups.load")
	nominal, nominalErr := parseNUTNumber(vars, "ups.realpower.nominal")
	if loadErr != nil || nominalErr != nil {
		return 0, errors.New("UPS reports neither ups.realpower nor ups.load with ups.realpower.nominal")
	}
	return uint(math.Round(max(load*nominal/100, 0))), nil
}

func parseNUTNumber(vars map[string]string, name string) (float64, error) {
	s, ok := vars[name]
	if !ok {
		return 0, fmt.Errorf("UPS doesn't report %s", name)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q", name, s)
	}
	return v, nil
}
