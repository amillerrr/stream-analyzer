// Package monitor runs stream-analyzer: per-channel playlist polling and
// segment checks, the rolling evidence buffer, incidents, the manual trigger
// and the health log.
package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/config"
)

// Options configure a Monitor. Only Config is required.
type Options struct {
	Config config.Config
	Logger *slog.Logger
	// Client fetches playlists and segments; nil uses a pooled default.
	Client *http.Client
	// Now is the clock for incident timing; nil uses time.Now.
	Now func() time.Time
	// OnSegment, if set, is called after each segment has been processed.
	OnSegment func(channel string, rec SegmentRecord)
	// SegmentRetries is how many times a failed segment fetch is retried
	// (default 2).
	SegmentRetries int
	// RetryDelay is the pause before the first retry; it doubles after that
	// (default 500ms).
	RetryDelay time.Duration
	// InitialSegments is how many segments of the first playlist are fetched
	// (default 3).
	InitialSegments int
	// OriginRetryDelay is the pause before the single retry of a segment
	// the origin refused, with any 4xx or 5xx (default 1s).
	OriginRetryDelay time.Duration
	// BlackDetector replaces ffmpeg blackdetect; nil uses ffmpeg.
	BlackDetector func(ctx context.Context, path string) ([]blackdetect.Interval, error)
	// FreeSpace returns the free bytes on dir's filesystem; nil asks the
	// operating system.
	FreeSpace func(dir string) (uint64, error)
}

// Monitor watches every configured channel.
type Monitor struct {
	cfg         config.Config
	opts        Options
	log         *slog.Logger
	client      *http.Client
	now         func() time.Time
	channels    []*Channel
	byName      map[string]*Channel
	incidents   *incidents
	ffmpeg      chan struct{} // limits concurrent ffmpeg processes
	csvMu       sync.Mutex    // serializes data/health.csv and data/events.csv appends
	bufferDir   string
	incidentDir string
	ctx         context.Context // Run's context; nil before Run
}

// New builds a Monitor from its options.
func New(o Options) (*Monitor, error) {
	if len(o.Config.Channels) == 0 {
		return nil, errors.New("no channels configured")
	}
	// config.Load checks these too; they matter enough to check here.
	if err := config.CheckChannelNames(o.Config.Channels); err != nil {
		return nil, err
	}
	if err := config.CheckUserAgent(o.Config.UserAgent); err != nil {
		return nil, err
	}
	o.SegmentRetries = cmp.Or(o.SegmentRetries, 2)
	o.RetryDelay = cmp.Or(o.RetryDelay, 500*time.Millisecond)
	o.InitialSegments = cmp.Or(o.InitialSegments, 3)
	o.OriginRetryDelay = cmp.Or(o.OriginRetryDelay, time.Second)
	m := &Monitor{
		cfg:         o.Config,
		opts:        o,
		log:         cmp.Or(o.Logger, slog.Default()),
		client:      o.Client,
		now:         o.Now,
		byName:      map[string]*Channel{},
		ffmpeg:      make(chan struct{}, max(1, o.Config.Blackdetect.Workers)),
		bufferDir:   filepath.Join(o.Config.DataDir, "buffer"),
		incidentDir: filepath.Join(o.Config.DataDir, "incidents"),
	}
	if m.client == nil {
		m.client = newHTTPClient(o.Config.TLSMaxVersion)
	}
	if m.now == nil {
		m.now = time.Now
	}
	for _, cc := range o.Config.Channels {
		c := newChannel(m, cc)
		m.channels = append(m.channels, c)
		m.byName[cc.Name] = c
	}
	m.incidents = newIncidents(m)
	return m, nil
}

// Run monitors until ctx is canceled, then writes a last health line for
// each channel and closes any open incidents. It refuses to start on a data
// directory another monitor is using.
func (m *Monitor) Run(ctx context.Context) error {
	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return err
	}
	unlock, err := lockDataDir(m.cfg.DataDir)
	switch {
	case errors.Is(err, errCannotLock):
		m.log.Warn("cannot lock the data directory; make sure no other stream-analyzer uses it", "error", err)
	case err != nil:
		return err
	}
	defer unlock()
	for _, d := range []string{m.bufferDir, m.incidentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	m.ctx = ctx
	m.incidents.recover()

	capture := "off"
	srv := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if m.cfg.Listen != "" {
		ln, err := net.Listen("tcp", m.cfg.Listen)
		if err != nil {
			return fmt.Errorf("manual trigger listener: %w", err)
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.log.Error("the manual trigger listener stopped: POST /capture no longer works", "error", err)
			}
		}()
		capture = "POST http://" + ln.Addr().String() + "/capture?channel=NAME"
	}
	m.log.Info("stream-analyzer started", "channels", len(m.channels), "data_dir", m.cfg.DataDir, "capture", capture)

	var wg sync.WaitGroup
	for _, c := range m.channels {
		wg.Go(func() { c.run(ctx) })
	}
	wg.Go(func() { m.incidents.run(ctx) })
	wg.Go(func() { m.healthLoop(ctx) })
	<-ctx.Done()

	// Stop taking captures first, then stop the channels, then close every
	// open incident. A capture racing the shutdown is refused (503) rather
	// than opening an incident nothing would close.
	stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(stop)
	wg.Wait()
	// The minute the monitor stopped in would otherwise go unrecorded.
	for _, c := range m.channels {
		c.logFinalHealth()
	}
	m.incidents.shutdown()
	m.log.Info("stream-analyzer stopped")
	return nil
}

// runContext is the context rendition fetchers hang off.
func (m *Monitor) runContext() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// detectBlack runs blackdetect, holding one of the ffmpeg worker slots.
// ffmpeg reports every black run (d=0); the monitor applies d to whole
// runs, joined across segments (see keepBlack).
func (m *Monitor) detectBlack(ctx context.Context, path string) ([]blackdetect.Interval, error) {
	select {
	case m.ffmpeg <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-m.ffmpeg }()
	if m.opts.BlackDetector != nil {
		return m.opts.BlackDetector(ctx, path)
	}
	o := m.cfg.Blackdetect.Options
	o.Duration = 0
	return blackdetect.Detect(ctx, path, o)
}

// blackRuns runs blackdetect on a saved segment and places the runs it
// finds in seg's PTS; frame is the video frame duration in ticks. Runs
// shorter than d that can't join a neighbour are dropped (see keepBlack).
func (m *Monitor) blackRuns(ctx context.Context, path string, seg *analysis.Segment, frame int64) ([]BlackInterval, error) {
	iv, err := m.detectBlack(ctx, path)
	if err != nil {
		return nil, err
	}
	return keepBlack(placeBlack(seg, iv, frame), m.cfg.Blackdetect.Duration), nil
}

// safely runs f, turning a panic into an error with the stack in the log,
// so the incident and health loops outlive a bug.
func (m *Monitor) safely(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("recovered from a panic; carrying on", "in", what, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	f()
}

// freeSpace returns the free bytes on the data directory's filesystem.
func (m *Monitor) freeSpace() (uint64, error) {
	if m.opts.FreeSpace != nil {
		return m.opts.FreeSpace(m.cfg.DataDir)
	}
	return diskFree(m.cfg.DataDir)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
