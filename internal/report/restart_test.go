package report

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A segment tagged CUE-OUT then CUE-IN (OUT+IN, in playlist order) ends
// the open break before starting the next, as IN+OUT does: two breaks, not
// an empty one and a lost one.
func TestReportOutPlusInEndsTheBreakFirst(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1069)
	s.cue("alpha", 1010, "OUT", "2.002")
	for seq := uint64(1011); seq < 1050; seq++ {
		if seq == 1030 {
			s.cues = append(s.cues, pick(cueHeader, map[string]string{"time_utc": stamp(at(seq)), "channel": "alpha",
				"seq": fmt.Sprint(seq), "scte35_tag": "OUT+IN", "extinf": "2.002",
				"tags": "#EXT-X-CUE-OUT:120.000 | #EXT-X-CUE-IN"}))
			continue
		}
		s.cue("alpha", seq, "CONT", "6")
	}
	s.cue("alpha", 1050, "IN", "6")
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(7 * time.Minute)}), "alpha")
	wantLines(t, alpha, "breaks 2, every 2.0 min", "break length 120 s declared; 116 s from CUE-OUT to CUE-IN")
}

// After a restart the first playlist's segments are processed again, and
// the health lines count them twice; the report counts each once.
func TestReportMonitoredCountsEachSegmentOnce(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	// Restarted at 1052: 1047-1049 processed again, then on to 1069.
	s.healthRow("alpha", 1052, 6, 0, at(1052).Add(5*time.Second))
	s.monitored("alpha", 1053, 1069)
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(7 * time.Minute)}), "alpha")
	wantLines(t, alpha, "monitored 70 segments, 0 monitor gaps")
}

// A stream reset starts the numbers over. Rows after it are a new run of
// numbers, not the same segments: the break before the reset and the one
// after it, on the same numbers, are both counted.
func TestReportKeepsCuesAcrossANumberReset(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1039)
	s.adBreak("alpha", 1010, 1030, false)
	// Ten minutes on, the packager restarts at 1000.
	later := func(row []string, seq uint64) []string {
		row[0] = stamp(at(seq).Add(10 * time.Minute))
		return row
	}
	n := len(s.cues)
	s.adBreak("alpha", 1010, 1030, false)
	for i := n; i < len(s.cues); i++ {
		var seq uint64
		fmt.Sscan(s.cues[i][2], &seq)
		s.cues[i] = later(s.cues[i], seq)
	}
	n = len(s.health)
	s.monitored("alpha", 1000, 1039)
	for i := n; i < len(s.health); i++ {
		var seq uint64
		fmt.Sscan(s.health[i][3], &seq)
		t0, _ := time.Parse(time.RFC3339, s.health[i][0])
		s.health[i][0] = t0.Add(10 * time.Minute).Format(time.RFC3339)
	}
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(20 * time.Minute)}), "alpha")
	wantLines(t, alpha, "splice points 2 CUE-OUT, 2 CUE-IN; 0 of 4 with an irregular segment")
}

// A last line without its newline may be a row the monitor was writing
// when it stopped: it is counted as bad, not read as a whole row.
func TestUnfinishedLastRowIsBad(t *testing.T) {
	var got []string
	bad, err := readCSV(strings.NewReader("time_utc,channel,seq\n2026-09-28T12:00:30Z,alpha,1004\n2026-09-28T12:00:36Z,alpha,10"), nil,
		func(r row) bool { got = append(got, r.col("seq")); return true })
	if err != nil || bad != 1 || !slices.Equal(got, []string{"1004"}) {
		t.Errorf("rows %v, bad %d, err %v: want only 1004, and the unfinished row bad", got, bad, err)
	}
}
