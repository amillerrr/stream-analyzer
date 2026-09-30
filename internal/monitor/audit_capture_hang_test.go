package monitor

// Scratch audit test: incident id selection loops forever when Stat fails
// with anything but "not exist".

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

func TestAuditStartLoopsForeverOnStatError(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	root := filepath.Join(cfg.DataDir, "incidents")
	os.MkdirAll(root, 0o755)
	os.Chmod(root, 0o000) // unreadable: Stat of any child fails with EACCES
	defer os.Chmod(root, 0o755)
	done := make(chan struct{})
	go func() {
		m.incidents.fault(m.channels[0], []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 1}}, SegmentRecord{Seq: 1, URI: "a"})
		close(done)
	}()
	select {
	case <-done:
		t.Log("fault() returned")
	case <-time.After(3 * time.Second):
		// Is incidents.mu still held? recording() would block.
		got := make(chan struct{})
		go func() { m.incidents.recording("test"); close(got) }()
		select {
		case <-got:
			t.Errorf("fault() still running after 3 s")
		case <-time.After(time.Second):
			t.Errorf("fault() still running after 3 s, holding incidents.mu: every channel's persist() and the health loop block")
		}
		os.Chmod(root, 0o755) // let it finish
		<-done
	}
}
