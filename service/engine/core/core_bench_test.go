package core

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// The two benchmarks below split the serial cost of the Kafka consume loop
// (decode → ProcessEvent on one goroutine) so each half can be measured alone.
// BenchmarkProcessEvent is the single-writer ceiling — no MQ, no JSON — the
// scope comparable to LMAX's Business Logic Processor figure.

const (
	benchMemberPool = 100                   // matches cmd/bench MEMBER_POOL default
	benchEventStep  = 12 * time.Microsecond // ~80K eps of event time
	benchChunk      = 4096                  // events generated per timer pause
)

// eventGen yields events with unique IDs and monotonic OccurredAt, so neither
// event_id dedup nor late-event routing short-circuits ProcessEvent.
type eventGen struct {
	next func(i int, at time.Time) *behaviorModel.BehaviorEvent
	i    int
	buf  []*behaviorModel.BehaviorEvent
	pos  int
}

var benchBaseTime = time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

func (g *eventGen) build(i int) *behaviorModel.BehaviorEvent {
	return g.next(i, benchBaseTime.Add(time.Duration(i)*benchEventStep))
}

// take hands out pre-built events; building the next chunk happens with the
// timer stopped so only ProcessEvent is measured.
func (g *eventGen) take(b *testing.B) *behaviorModel.BehaviorEvent {
	if g.pos == len(g.buf) {
		b.StopTimer()
		g.buf = g.buf[:0]
		for range benchChunk {
			g.buf = append(g.buf, g.build(g.i))
			g.i++
		}
		g.pos = 0
		b.StartTimer()
	}
	ev := g.buf[g.pos]
	g.pos++
	return ev
}

func benchMember(i int) string { return fmt.Sprintf("user-%04d", i%benchMemberPool+1) }

// withdrawGen mirrors the cmd/bench payload: CryptoWithdraw with a cycling amount.
func withdrawGen() *eventGen {
	return &eventGen{next: func(i int, at time.Time) *behaviorModel.BehaviorEvent {
		return withdraw(fmt.Sprintf("ev-%d", i), benchMember(i), float64(i%10000)+0.5, at)
	}}
}

// loginGen cycles failed → failed → success per member so the fraud pattern
// keeps advancing and completing instead of idling.
func loginGen() *eventGen {
	steps := [...]string{"login_failed", "login_failed", "login_success"}
	return &eventGen{next: func(i int, at time.Time) *behaviorModel.BehaviorEvent {
		step := steps[(i/benchMemberPool)%len(steps)]
		return behaviorEvent(fmt.Sprintf("ev-%d", i), benchMember(i), step, at)
	}}
}

func BenchmarkProcessEvent(b *testing.B) {
	b.Run("aggregate", func(b *testing.B) {
		c := NewCore(0, buildWithdrawRuleSet(b))
		gen := withdrawGen()
		b.ReportAllocs()
		for b.Loop() {
			c.ProcessEvent(gen.take(b))
		}
	})

	b.Run("cep", func(b *testing.B) {
		c := NewCore(0, &ruleModel.CompiledRuleSet{})
		if err := c.AddPattern(fraudPattern()); err != nil {
			b.Fatal(err)
		}
		gen := loginGen()
		b.ReportAllocs()
		for b.Loop() {
			c.ProcessEvent(gen.take(b))
		}
	})
}

func BenchmarkDecodeEvent(b *testing.B) {
	gen := withdrawGen()
	payloads := make([][]byte, benchChunk)
	for i := range payloads {
		data, err := json.Marshal(gen.build(i))
		if err != nil {
			b.Fatal(err)
		}
		payloads[i] = data
	}

	b.SetBytes(int64(len(payloads[0])))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if _, err := decodeEvent(payloads[i%len(payloads)]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}
