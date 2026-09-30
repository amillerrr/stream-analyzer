package monitor

import (
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

// Report is an incident's report.json. It is rewritten as the incident
// grows and finalized when it closes.
type Report struct {
	ID          string    `json:"id"`
	Channel     string    `json:"channel"`
	Status      string    `json:"status"` // open, closed or interrupted
	OpenedAt    time.Time `json:"opened_at"`
	LastFaultAt time.Time `json:"last_fault_at"`
	ClosedAt    time.Time `json:"closed_at,omitzero"`
	// CloseReason is post_roll_elapsed, max_duration or shutdown.
	CloseReason string     `json:"close_reason,omitempty"`
	Stream      StreamInfo `json:"stream"`
	FaultTypes  []string   `json:"fault_types"`
	// FaultCount counts every fault, including any beyond the listed limit.
	FaultCount int `json:"fault_count"`
	// SequenceNumbers are the segments the faults involve.
	SequenceNumbers []uint64      `json:"sequence_numbers"`
	Faults          []FaultRecord `json:"faults"`
	// OriginNotes are the origin's rule-breaking habits (see OriginNote)
	// seen in the incident's evidence: while it was open, and in its
	// pre-roll.
	OriginNotes []OriginNote `json:"origin_notes,omitempty"`
	// Discontinuities are the segments (among Segments) preceded by
	// EXT-X-DISCONTINUITY.
	Discontinuities []uint64 `json:"discontinuities"`
	// SCTE35 lists the ad-signaling tags on the segments in Segments.
	SCTE35      []TagRecord       `json:"scte35"`
	MonitorGaps []analysis.Gap    `json:"monitor_gaps"`
	Segments    []SegmentRecord   `json:"segments"`
	Renditions  []RenditionReport `json:"renditions,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	// BlackRuns lists every black run blackdetect found (d and longer) in
	// the segments above, whether or not it was long enough to open an
	// incident.
	BlackRuns []BlackRun `json:"black_runs"`
	// Files lists every evidence file in the incident directory, with its
	// size and SHA-256, once the incident has closed.
	Files []EvidenceFile `json:"files,omitempty"`
}

// EvidenceFile is one file in an incident directory.
type EvidenceFile struct {
	Path   string `json:"path"` // relative to the incident directory, with /
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// BlackRun is one black run in one segment: seconds from the segment's
// first frame, and PTS.
type BlackRun struct {
	Seq      uint64  `json:"seq"`
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	Duration float64 `json:"duration_s"`
	StartPTS uint64  `json:"start_pts,omitzero"`
	EndPTS   uint64  `json:"end_pts,omitzero"`
}

// StreamInfo identifies what was being monitored.
type StreamInfo struct {
	URL              string   `json:"url"`
	MediaPlaylistURL string   `json:"media_playlist_url"`
	Rendition        string   `json:"rendition,omitempty"`
	Codecs           string   `json:"codecs,omitempty"`
	TargetDurationS  float64  `json:"target_duration_s,omitzero"`
	AVBaselineMs     *float64 `json:"av_baseline_ms,omitempty"`
}

// FaultRecord is one fault with its context.
type FaultRecord struct {
	Type       string    `json:"type"`
	DetectedAt time.Time `json:"detected_at"`
	Seq        uint64    `json:"seq"`
	PrevSeq    *uint64   `json:"prev_seq,omitempty"`
	// URI is the playlist URI of the segment the fault is on.
	URI     string         `json:"uri,omitempty"`
	Message string         `json:"message"`
	Values  map[string]any `json:"values,omitempty"`
	// Discontinuity is whether EXT-X-DISCONTINUITY preceded the segment.
	Discontinuity bool `json:"discontinuity"`
	// SCTE35Nearby lists ad-signaling tags within two segments of this one.
	SCTE35Nearby []TagRecord `json:"scte35_nearby,omitempty"`
	// Renditions says, per other rendition, whether the same segments show
	// the same fault: reproduced (the same fault, at the same boundary, of
	// about the same size), different (the same type of fault but another
	// size, or a black run shorter than trigger_min), not_reproduced,
	// inconclusive (the check could not run there, or the rendition lists
	// another segment before it), incomplete (the previous segment is
	// missing), or the segment's status: pending, failed, expired,
	// not_listed or mismatched. A stall is judged by the rendition's
	// playlist while the monitored one was stalled.
	Renditions map[string]string `json:"renditions,omitempty"`
	// RenditionValues are each other rendition's own measurements behind
	// its verdict.
	RenditionValues map[string]map[string]any `json:"rendition_values,omitempty"`
	// PreRoll (report.v2.json only) marks a fault found before the original
	// incident opened, in its pre-roll. A monitor running the current checks
	// would have reported it then: in an incident of its own, or in one
	// already open.
	PreRoll bool `json:"pre_roll,omitzero"`
}

// OriginNote is origin behaviour that breaks a rule for live playlists but
// is the origin's normal practice: it is logged, written to
// data/origin_notes.csv, counted and listed, but opens no incident.
type OriginNote struct {
	DetectedAt time.Time `json:"detected_at"`
	// Seq and URI are the entry concerned, the first when there are
	// several.
	Seq    uint64 `json:"seq"`
	URI    string `json:"uri,omitempty"`
	Reason string `json:"reason"` // as for playlist_violation
	Detail string `json:"detail"`
	// Values holds the measurements and the two playlists' files.
	Values map[string]any `json:"values,omitempty"`
	// PreRoll: noted before the incident opened, in its pre-roll.
	PreRoll bool `json:"pre_roll,omitzero"`
}

// TagRecord is the tags on one segment.
type TagRecord struct {
	Seq  uint64   `json:"seq"`
	Tags []string `json:"tags"`
}

// RenditionReport is what was fetched from another rendition.
type RenditionReport struct {
	Label       string             `json:"label"`
	PlaylistURL string             `json:"playlist_url"`
	Segments    []RenditionSegment `json:"segments"`
}

// RenditionSegment is one segment fetched from another rendition, checked
// the same way as the monitored one.
type RenditionSegment struct {
	// Seq is the origin's number, as for the monitored rendition.
	Seq uint64 `json:"seq"`
	// Status is pending, fetched, failed (last attempt failed; retried while
	// listed), expired (left the playlist first), not_listed (this
	// rendition's playlist skips the number) or mismatched (it does not
	// start at the monitored copy's media time).
	Status string `json:"status"`
	URI    string `json:"uri,omitempty"`
	// MediaOffsetMs is how far this copy starts from the monitored one
	// (first video DTS): 0 for the same media.
	MediaOffsetMs *float64          `json:"media_offset_ms,omitempty"`
	File          string            `json:"file,omitempty"`
	Fetch         *FetchMeta        `json:"fetch,omitempty"`
	Analysis      *analysis.Summary `json:"analysis,omitempty"`
	Black         []BlackInterval   `json:"black_intervals,omitempty"`
	Faults        []string          `json:"faults,omitempty"`
	// Unchecked lists fault types that could not be checked on this
	// segment, e.g. because it was not usable TS or ffmpeg failed.
	Unchecked []string `json:"unchecked,omitempty"`
	Error     string   `json:"error,omitempty"`
}
