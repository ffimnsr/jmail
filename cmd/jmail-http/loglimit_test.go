package main

import (
	"testing"
	"time"
)

func TestLogLimiterAllowsTheBurstThenDrops(t *testing.T) {
	limiter := newLogLimiter(10) // burst of 20
	now := time.Now()

	allowed := 0
	for i := 0; i < 50; i++ {
		if ok, _ := limiter.allow(now); ok {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("allowed %d lines, want the burst of 20", allowed)
	}
}

func TestLogLimiterReportsSuppressed(t *testing.T) {
	limiter := newLogLimiter(1) // burst of 2, one token per second
	now := time.Now()

	// Burn the burst.
	for i := 0; i < 2; i++ {
		if ok, _ := limiter.allow(now); !ok {
			t.Fatalf("line %d should have been allowed", i)
		}
	}

	// These are the ones that get dropped, and counted.
	for i := 0; i < 5; i++ {
		if ok, _ := limiter.allow(now); ok {
			t.Fatalf("line %d should have been dropped", i)
		}
	}

	// A second later one token is back; the line that gets through must report
	// how many were lost in between.
	ok, suppressed := limiter.allow(now.Add(time.Second))
	if !ok {
		t.Fatal("expected the refilled token to allow a line")
	}
	if suppressed != 5 {
		t.Fatalf("suppressed = %d, want 5", suppressed)
	}
}

func TestLogLimiterEmitsSummaryDuringALongFlood(t *testing.T) {
	limiter := newLogLimiter(1)
	now := time.Now()

	limiter.allow(now)
	limiter.allow(now)

	// Well past the report interval, keep flooding with no pause. The first two
	// calls consume the refilled tokens, then a summary has to be emitted even
	// though no normal line is allowed.
	flood := now.Add(logSuppressionReportInterval + time.Second)
	reported := false
	for i := 0; i < 20; i++ {
		if ok, suppressed := limiter.allow(flood); ok && suppressed > 0 {
			reported = true
			break
		}
	}
	if !reported {
		t.Fatal("expected a suppression summary while the flood continues")
	}
}

func TestLogLimiterDisabledIsAlwaysAllowed(t *testing.T) {
	if newLogLimiter(0) != nil {
		t.Fatal("a zero rate should disable sampling entirely")
	}
	var nilLimiter *logLimiter
	if ok, _ := nilLimiter.allow(time.Now()); !ok {
		t.Fatal("a nil limiter should allow everything")
	}
}
