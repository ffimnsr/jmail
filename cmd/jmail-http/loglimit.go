package main

import (
	"math"
	"sync"
	"time"
)

// logSuppressionReportInterval is how often a drop report is emitted while a
// flood continues, so suppressed lines are never silent for long.
const logSuppressionReportInterval = 5 * time.Second

// logLimiter caps access-log lines per second. Without it, a client that ignores
// 429s writes one line per rejected request, which is enough to fill the
// container log and bury everything else.
type logLimiter struct {
	mu         sync.Mutex
	rate       float64
	burst      float64
	tokens     float64
	last       time.Time
	suppressed int
	lastReport time.Time
}

// newLogLimiter returns nil when rate is not positive, which means unlimited.
func newLogLimiter(rate float64) *logLimiter {
	if rate <= 0 {
		return nil
	}

	burst := math.Max(2, rate*2)
	now := time.Now()
	return &logLimiter{rate: rate, burst: burst, tokens: burst, last: now, lastReport: now}
}

// allow reports whether to write the line, and how many lines were suppressed
// since the last report. A non-zero count is returned on the line that carries
// the report, either the next allowed request or a periodic summary while the
// flood continues.
func (l *logLimiter) allow(now time.Time) (bool, int) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = math.Min(l.burst, l.tokens+elapsed*l.rate)
		l.last = now
	}

	if l.tokens >= 1 {
		l.tokens--
		suppressed := l.suppressed
		l.suppressed = 0
		return true, suppressed
	}

	l.suppressed++
	if now.Sub(l.lastReport) >= logSuppressionReportInterval {
		l.lastReport = now
		suppressed := l.suppressed
		l.suppressed = 0
		return true, suppressed
	}
	return false, 0
}
