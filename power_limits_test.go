package main

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestPlanPowerLimits(t *testing.T) {
	busy := func(limit uint) gpuBudget { return gpuBudget{base: 340, min: 100, usage: limit - 5, busy: true} }
	idle := func(uint) gpuBudget { return gpuBudget{base: 340, min: 100, usage: 20} }

	tests := []struct {
		name string
		gpus []gpuBudget
		cap  uint
		want []uint
	}{
		{"no cap gives base limits", []gpuBudget{busy(250), idle(250)}, 0, []uint{340, 340}},
		{"bases under the cap are kept", []gpuBudget{busy(340), busy(340)}, 680, []uint{340, 340}},
		{"busy GPUs share the cap evenly", []gpuBudget{busy(340), busy(340)}, 600, []uint{300, 300}},
		{"busy GPU keeps its base while the other idles", []gpuBudget{busy(340), idle(340)}, 600, []uint{340, 260}},
		{"idle GPU ramping up at its limit gets an even share", []gpuBudget{busy(340), busy(260)}, 600, []uint{300, 300}},
		{"busy GPU recovers its base once the other idles", []gpuBudget{busy(300), idle(300)}, 600, []uint{340, 260}},
		{"cooler tier below its share is kept", []gpuBudget{{base: 250, min: 100, usage: 245, busy: true}, busy(340)}, 600, []uint{250, 340}},
		{"hardware minimums win over the cap", []gpuBudget{busy(340), busy(340)}, 150, []uint{100, 100}},
		{"base below the minimum is raised to it", []gpuBudget{{base: 50, min: 100}}, 0, []uint{100}},
		{"previously capped GPUs are restored", []gpuBudget{idle(200), idle(200)}, 680, []uint{340, 340}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planPowerLimits(tt.gpus, tt.cap); !slices.Equal(got, tt.want) {
				t.Errorf("planPowerLimits() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The sum of limits must never exceed the cap (unless the hardware minimums
// do), and each limit must stay within [min, max(base, min)].
func TestPlanPowerLimitsInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		n := 1 + r.IntN(4)
		gpus := make([]gpuBudget, n)
		var minSum uint
		for i := range gpus {
			minL := uint(50 + r.IntN(100))
			gpus[i] = gpuBudget{
				base:  uint(r.IntN(500)),
				min:   minL,
				usage: uint(r.IntN(400)),
				busy:  r.IntN(2) == 0,
			}
			minSum += minL
		}
		totalCap := uint(r.IntN(1500))

		limits := planPowerLimits(gpus, totalCap)
		var sum uint
		for i, l := range limits {
			sum += l
			if l < gpus[i].min || l > max(gpus[i].base, gpus[i].min) {
				t.Fatalf("limit %d out of range for %+v (cap %d): %v", l, gpus[i], totalCap, limits)
			}
		}
		if totalCap > 0 && sum > max(totalCap, minSum) {
			t.Fatalf("limits %v sum to %d, over cap %d for %+v", limits, sum, totalCap, gpus)
		}
	}
}

// Feeds each plan back in as the next cycle's limits, with GPUs drawing up to
// a natural demand, and checks the limits settle rather than flip-flopping.
func TestPlanPowerLimitsSettles(t *testing.T) {
	tests := []struct {
		name    string
		natural []uint // what each GPU would draw unlimited
		cap     uint
		want    []uint
	}{
		{"steady light GPU beside a busy one", []uint{200, 400}, 500, []uint{230, 270}},
		{"three GPUs, one light", []uint{150, 400, 400}, 700, []uint{180, 260, 260}},
		{"both busy", []uint{400, 400}, 600, []uint{300, 300}},
		{"both idle", []uint{20, 20}, 600, []uint{300, 300}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := make([]uint, len(tt.natural))
			for i := range limits {
				limits[i] = 350
			}
			var history [][]uint
			for range 6 {
				gpus := make([]gpuBudget, len(limits))
				for i, n := range tt.natural {
					limited := n >= limits[i]
					usage := n
					if limited {
						usage = limits[i] - 5 // averaged draw reads a little under the limit
					}
					gpus[i] = gpuBudget{base: 350, min: 100, usage: usage,
						busy: powerLimited(limited, usage, limits[i])}
				}
				limits = planPowerLimits(gpus, tt.cap)
				history = append(history, limits)
			}
			for _, h := range history[2:] {
				if !slices.Equal(h, tt.want) {
					t.Fatalf("limits did not settle on %v: %v", tt.want, history)
				}
			}
		})
	}
}

func TestPowerLimited(t *testing.T) {
	tests := []struct {
		name           string
		throttled      bool
		usage, current uint
		want           bool
	}{
		{"throttle reason set", true, 100, 300, true},
		{"drawing at the limit", false, 295, 300, true},
		{"granted headroom is not mistaken for busy", false, 200, 230, false},
		{"idle", false, 20, 300, false},
	}
	for _, tt := range tests {
		if got := powerLimited(tt.throttled, tt.usage, tt.current); got != tt.want {
			t.Errorf("%s: powerLimited() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTempTierLimit(t *testing.T) {
	tiers := TempPowerLimits{LowTemp: 80, MediumTemp: 86, LowTempLimit: 340, MediumTempLimit: 300, HighTempLimit: 250}
	for temp, want := range map[uint]uint{40: 340, 80: 340, 81: 300, 86: 300, 87: 250} {
		if got := tempTierLimit(tiers, temp); got != want {
			t.Errorf("tempTierLimit(%d) = %d, want %d", temp, got, want)
		}
	}
}

func TestEffectiveTotalCap(t *testing.T) {
	defer func(c uint, g *upsGuard) { totalPowerCap, upsGuardInstance = c, g }(totalPowerCap, upsGuardInstance)

	guard := &upsGuard{}
	upsGuardInstance = guard
	for _, tt := range []struct{ total, ups, want uint }{
		{0, 0, 0},
		{680, 0, 680},
		{0, 600, 600},
		{680, 600, 600},
		{500, 600, 500},
	} {
		totalPowerCap = tt.total
		guard.capW.Store(uint32(tt.ups))
		if got := effectiveTotalCap(); got != tt.want {
			t.Errorf("total %d, ups %d: effectiveTotalCap() = %d, want %d", tt.total, tt.ups, got, tt.want)
		}
	}
}
