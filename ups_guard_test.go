package main

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseUPSGuardConfigDisabledWithoutHost(t *testing.T) {
	_, ok, err := parseUPSGuardConfig(envMap(map[string]string{"UPS_POWER_HIGH": "1100"}))
	if ok || err != nil {
		t.Errorf("got ok=%v err=%v, want disabled without error", ok, err)
	}
}

func TestParseUPSGuardConfigDefaults(t *testing.T) {
	cfg, ok, err := parseUPSGuardConfig(envMap(map[string]string{"UPS_NUT_HOST": "ups.local"}))
	if !ok || err != nil {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	if cfg.nut.addr != "ups.local:3493" || cfg.pollInterval != 5*time.Second || cfg.cooldown != 10*time.Minute {
		t.Errorf("unexpected defaults %+v", cfg)
	}
	if cfg.highW != 0 || cfg.lowW != 0 || cfg.gpuCapW != 0 || cfg.upsName != "" {
		t.Errorf("thresholds, cap and UPS name should default to derived values, got %+v", cfg)
	}
}

func TestParseUPSGuardConfigExplicit(t *testing.T) {
	cfg, ok, err := parseUPSGuardConfig(envMap(map[string]string{
		"UPS_NUT_HOST": "fd00::1", "UPS_NUT_PORT": "3494", "UPS_NUT_NAME": "eaton",
		"UPS_NUT_USERNAME": "mon", "UPS_NUT_PASSWORD": "pw",
		"UPS_POLL_INTERVAL": "2", "UPS_POWER_HIGH": "1100", "UPS_POWER_LOW": "600",
		"UPS_COOLDOWN": "300", "UPS_GPU_POWER_CAP": "500",
	}))
	if !ok || err != nil {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	want := upsGuardConfig{
		nut:          nutClient{addr: "[fd00::1]:3494", username: "mon", password: "pw", timeout: 5 * time.Second},
		upsName:      "eaton",
		pollInterval: 2 * time.Second,
		highW:        1100,
		lowW:         600,
		cooldown:     5 * time.Minute,
		gpuCapW:      500,
	}
	if cfg != want {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestParseUPSGuardConfigErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"low not below high": {"UPS_POWER_HIGH": "600", "UPS_POWER_LOW": "600"},
		"non-numeric":        {"UPS_POWER_HIGH": "1.1kW"},
		"negative":           {"UPS_COOLDOWN": "-1"},
		"zero poll interval": {"UPS_POLL_INTERVAL": "0"},
	} {
		env["UPS_NUT_HOST"] = "ups.local"
		if _, _, err := parseUPSGuardConfig(envMap(env)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNewUPSGuardCap(t *testing.T) {
	g, err := newUPSGuard(upsGuardConfig{}, 740)
	if err != nil || g.cfg.gpuCapW != 370 {
		t.Errorf("default cap: got %+v, %v; want 370 W", g, err)
	}
	g, err = newUPSGuard(upsGuardConfig{gpuCapW: 600}, 740)
	if err != nil || g.cfg.gpuCapW != 600 {
		t.Errorf("explicit cap: got %+v, %v; want 600 W", g, err)
	}
	if _, err := newUPSGuard(upsGuardConfig{}, 0); err == nil {
		t.Error("expected an error with no cap and no GPU defaults")
	}
}

func TestUPSGuardHysteresis(t *testing.T) {
	g := &upsGuard{cfg: upsGuardConfig{gpuCapW: 600, cooldown: 10 * time.Minute}}
	start := time.Now()
	step := func(minutes float64, powerW uint, wantCap uint) {
		t.Helper()
		g.observe(powerW, 1100, 600, start.Add(time.Duration(minutes*float64(time.Minute))))
		if got := g.activeCap(); got != wantCap {
			t.Fatalf("at %.1f min drawing %d W: cap = %d, want %d", minutes, powerW, got, wantCap)
		}
	}

	step(0, 1099, 0)   // just under the high threshold
	step(1, 1100, 600) // trips at the threshold
	step(2, 900, 600)  // between thresholds: stays capped, no cooldown
	step(3, 500, 600)  // cooldown starts
	step(12, 550, 600) // 9 minutes low
	step(12.5, 650, 600)
	step(13, 500, 600) // cooldown restarted by the 650 W reading
	step(22.9, 500, 600)
	step(23, 500, 0) // 10 minutes low: released
	step(24, 900, 0) // stays released until the high threshold
}

func TestUPSGuardNilIsInactive(t *testing.T) {
	var g *upsGuard
	if g.activeCap() != 0 {
		t.Error("nil guard should report no cap")
	}
}

func TestUPSPowerWatts(t *testing.T) {
	tests := []struct {
		vars map[string]string
		want uint
		err  bool
	}{
		{map[string]string{"ups.realpower": "137"}, 137, false},
		{map[string]string{"ups.realpower": "137.6", "ups.load": "50", "ups.realpower.nominal": "1200"}, 138, false},
		{map[string]string{"ups.load": "13", "ups.realpower.nominal": "1200"}, 156, false},
		{map[string]string{"ups.load": "13"}, 0, true},
		{map[string]string{"ups.realpower": "n/a"}, 0, true},
		{map[string]string{}, 0, true},
	}
	for _, tt := range tests {
		got, err := upsPowerWatts(tt.vars)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("upsPowerWatts(%v) = %d, %v; want %d, err %v", tt.vars, got, err, tt.want, tt.err)
		}
	}
}

func TestUPSGuardThresholds(t *testing.T) {
	g := &upsGuard{}
	high, low, err := g.thresholds(map[string]string{"ups.realpower.nominal": "1200"})
	if err != nil || high != 1080 || low != 600 {
		t.Errorf("derived thresholds = %d, %d, %v; want 1080, 600", high, low, err)
	}

	g.cfg.highW, g.cfg.lowW = 1100, 600
	if high, low, err := g.thresholds(map[string]string{}); err != nil || high != 1100 || low != 600 {
		t.Errorf("explicit thresholds = %d, %d, %v; want 1100, 600", high, low, err)
	}

	g.cfg.highW, g.cfg.lowW = 0, 700
	if _, _, err := g.thresholds(map[string]string{}); err == nil {
		t.Error("expected an error deriving the high threshold without a UPS rating")
	}

	g.cfg.highW, g.cfg.lowW = 500, 0 // derived low (600) above explicit high
	if _, _, err := g.thresholds(map[string]string{"ups.realpower.nominal": "1200"}); err == nil {
		t.Error("expected an error when the derived low threshold is above the high threshold")
	}
}

func TestUPSGuardPollAgainstUPSD(t *testing.T) {
	f, c := newFakeUPSD(t, map[string]string{"ups.realpower": "200", "ups.realpower.nominal": "1200"})
	g, err := newUPSGuard(upsGuardConfig{nut: c, pollInterval: time.Second, highW: 1100, lowW: 600, cooldown: time.Minute}, 740)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	g.pollOnce(now)
	if g.cfg.upsName != "eaton" || g.activeCap() != 0 || g.lastErr != "" {
		t.Fatalf("after first poll: name %q, cap %d, err %q", g.cfg.upsName, g.activeCap(), g.lastErr)
	}

	f.set("eaton", "ups.realpower", "1150")
	g.pollOnce(now.Add(time.Second))
	if g.activeCap() != 370 {
		t.Fatalf("cap = %d, want 370 after high draw", g.activeCap())
	}

	// A failed read keeps the cap and restarts the cooldown.
	f.set("eaton", "ups.realpower", "100")
	g.pollOnce(now.Add(2 * time.Second))
	good := g.cfg.nut
	g.cfg.nut.addr = "127.0.0.1:1"
	g.pollOnce(now.Add(70 * time.Second))
	if g.activeCap() != 370 || !g.lowSince.IsZero() || !strings.Contains(g.lastErr, "connecting") {
		t.Fatalf("after failed poll: cap %d, lowSince %v, err %q", g.activeCap(), g.lowSince, g.lastErr)
	}

	g.cfg.nut = good
	g.pollOnce(now.Add(71 * time.Second))
	g.pollOnce(now.Add(131 * time.Second))
	if g.activeCap() != 0 || g.lastErr != "" {
		t.Fatalf("after cooldown: cap %d, err %q", g.activeCap(), g.lastErr)
	}
}
