package main

import (
	"sort"
	"time"
)

// summary holds the latency distribution of one benchmark stage.
type summary struct {
	Count int
	Mean  time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

// summarize computes percentiles over a copy of durs (input order preserved).
// Percentile rank uses the nearest-rank method: p-th percentile = value at
// index ceil(p/100 * N) - 1 of the sorted samples.
func summarize(durs []time.Duration) summary {
	if len(durs) == 0 {
		return summary{}
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	rank := func(p float64) time.Duration {
		idx := int(float64(len(sorted))*p+0.999999) - 1 // ceil(N*p) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return summary{
		Count: len(sorted),
		Mean:  total / time.Duration(len(sorted)),
		P50:   rank(0.50),
		P95:   rank(0.95),
		P99:   rank(0.99),
		Max:   sorted[len(sorted)-1],
	}
}
