package forecast

import (
	"math"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// GetBurnRate calculates recent inference frequency and quota burn rate across rolling windows.
// Applies multi-window blended moving average and burst damping for realistic forecasting.
func GetBurnRate(pool *storage.Pool, nowOpt ...float64) BurnRateTelemetry {
	now := float64(time.Now().Unix())
	if len(nowOpt) > 0 && nowOpt[0] > 0 {
		now = nowOpt[0]
	}

	var validTS []float64
	if pool != nil {
		for _, e := range pool.GenerationEvents {
			if e.TS > 0 {
				validTS = append(validTS, float64(e.TS))
			}
		}
	}

	n15 := 0
	n60 := 0
	n240 := 0
	for _, ts := range validTS {
		diff := now - ts
		if diff <= 900 {
			n15++
		}
		if diff <= 3600 {
			n60++
		}
		if diff <= 14400 {
			n240++
		}
	}

	r15 := float64(n15) * 4.0
	r60 := float64(n60)
	r240 := float64(n240) / 4.0

	var rate float64
	var window string

	if n240 == 0 {
		rate = 0.0
		window = "idle"
	} else if n60 >= 10 {
		// Sustained session: damp high-frequency micro-bursts (max 2x 1h rate or at least 30)
		rBurst := math.Min(r15, math.Max(r60*2.0, 30.0))
		rate = 0.50*r60 + 0.30*rBurst + 0.20*r240
		window = "smoothed"
	} else if n15 >= 2 {
		rate = r15
		window = "15m"
	} else if n60 >= 1 {
		rate = r60
		window = "1h"
	} else {
		rate = r240
		window = "4h"
	}

	rate = math.Round(rate*10.0) / 10.0
	dailyPace := 0.0
	if rate > 0 {
		dailyPace = math.Round((rate/3.0)*10.0) / 10.0
	}

	return BurnRateTelemetry{
		GensLast15M:     n15,
		GensLast1H:      n60,
		GensLast4H:      n240,
		BurstRate15M:    math.Round(r15*10.0) / 10.0,
		SustainedRate1H: math.Round(r60*10.0) / 10.0,
		SustainedRate4H: math.Round(r240*10.0) / 10.0,
		GensPerHour:     rate,
		DailyPace:       dailyPace,
		IsIdle:          rate == 0.0,
		WindowUsed:      window,
	}
}
