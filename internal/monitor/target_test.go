package monitor

import (
	"testing"
	"time"
)

// However long a target duration the channel has, it is polled at least
// every 10 s and a segment fetch gives up within a minute.
func TestPollAndFetchTimesAreBounded(t *testing.T) {
	m := newTestMonitor(t, testConfig(t, "http://127.0.0.1:1/never.m3u8"), nil)
	c := m.channels[0]
	c.target = time.Hour
	if got := c.pollInterval(); got > 10*time.Second {
		t.Errorf("poll interval %v with a 1h target", got)
	}
	if got := segmentTimeout(time.Hour); got > time.Minute {
		t.Errorf("segment timeout %v with a 1h target", got)
	}
	if got := segmentTimeout(6 * time.Second); got != 18*time.Second {
		t.Errorf("segment timeout %v with a 6s target, want 18s", got)
	}
}

// A target duration outside 1-30 s (seconds mistaken for milliseconds, a
// garbled value) is not adopted.
func TestImplausibleTargetDurationIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		target float64
		want   time.Duration
	}{
		{7000, 6 * time.Second}, {0.001, 6 * time.Second}, {7, 7 * time.Second}, {30, 30 * time.Second},
	} {
		m := newTestMonitor(t, testConfig(t, "http://127.0.0.1:1/never.m3u8"), nil)
		c := m.channels[0]
		c.target = 6 * time.Second
		c.adoptTarget(tc.target)
		if c.targetDuration() != tc.want {
			t.Errorf("EXT-X-TARGETDURATION %g: target %v, want %v", tc.target, c.targetDuration(), tc.want)
		}
	}
}
