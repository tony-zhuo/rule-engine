package main

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	tests := []struct {
		name string
		durs []time.Duration
		p50  time.Duration
		p95  time.Duration
		p99  time.Duration
		max  time.Duration
	}{
		{
			name: "uniform 1..100ms",
			durs: rangeMillis(1, 100),
			p50:  50 * time.Millisecond,
			p95:  95 * time.Millisecond,
			p99:  99 * time.Millisecond,
			max:  100 * time.Millisecond,
		},
		{
			name: "single sample",
			durs: []time.Duration{7 * time.Millisecond},
			p50:  7 * time.Millisecond,
			p95:  7 * time.Millisecond,
			p99:  7 * time.Millisecond,
			max:  7 * time.Millisecond,
		},
		{
			name: "unsorted input",
			durs: []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond},
			p50:  20 * time.Millisecond,
			p95:  30 * time.Millisecond,
			p99:  30 * time.Millisecond,
			max:  30 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := summarize(tt.durs)
			if s.Count != len(tt.durs) {
				t.Fatalf("count = %d, want %d", s.Count, len(tt.durs))
			}
			if s.P50 != tt.p50 || s.P95 != tt.p95 || s.P99 != tt.p99 || s.Max != tt.max {
				t.Fatalf("got p50=%v p95=%v p99=%v max=%v, want p50=%v p95=%v p99=%v max=%v",
					s.P50, s.P95, s.P99, s.Max, tt.p50, tt.p95, tt.p99, tt.max)
			}
		})
	}
}

func TestSummarize_Empty(t *testing.T) {
	s := summarize(nil)
	if s.Count != 0 || s.P50 != 0 || s.Max != 0 {
		t.Fatalf("empty input should produce zero summary, got %+v", s)
	}
}

func rangeMillis(from, to int) []time.Duration {
	out := make([]time.Duration, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, time.Duration(i)*time.Millisecond)
	}
	return out
}
