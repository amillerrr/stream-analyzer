# stream-analyzer

stream-analyzer watches live HLS channels. It downloads every segment exactly
as the origin serves it, checks each one for timing problems, black video
and playlist errors, and saves the segments and playlists from around each
problem, with a report of what it found.

## How it works

1. It loads each channel's URL. From a master playlist it picks one
   rendition, the highest bandwidth by default, and watches it
   continuously.
2. It fetches that media playlist twice per target duration, compares it
   with the previous fetch, and saves every fetch with its response
   headers.
3. It downloads each new segment once, byte for byte, and checks it: PTS,
   DTS and PCR inside the segment and against the segment listed before
   it, audio against video, the length against EXTINF, and black video
   with ffmpeg's `blackdetect`.
4. It keeps the last 3 minutes of each channel's segments and playlists in
   a rolling buffer.
5. A fault opens an incident. The buffer is copied in as pre-roll,
   recording continues until 60 s after the last fault, and the same
   segments are fetched from the channel's other renditions to see whether
   they show the fault too.
6. Smaller irregularities go to CSV files without opening an incident, and
   each channel writes a health line every minute.

## Requirements

Go 1.27 or newer is needed only to build it (see `go.mod`). Black
detection runs `ffmpeg`, which has to be on `PATH`; without it, set
`blackdetect.enabled: false`. It runs on macOS and Linux and needs network
access to the streams. HTTPS uses the system's CA certificates.

Disk use follows the bitrate of the watched rendition. Each Mbps is about
7.5 MB a minute, so the default 3-minute buffer of a 4 Mbps channel is
about 90 MB, and an incident with three other renditions comes to 100 to
120 MB. `incident_storage_gb` (20 GB by default) caps the buffer and the
incidents together. The CSV files and the log add about 2 MB a day per
channel and are not rotated. For ten channels, 25 to 30 GB of free space
covers the cap, a few months of CSV files, and the 5 GB of free space
below which the monitor warns.

## Build

```bash
git clone https://github.com/amillerrr/stream-analyzer
cd stream-analyzer
go build -o stream-analyzer .
```

Or install it without cloning the repository. The binary goes to
`$(go env GOPATH)/bin`, or to `$GOBIN` if that is set:

```bash
go install github.com/amillerrr/stream-analyzer@latest
```

On a Mac, cross-compile static binaries for Linux with:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o stream-analyzer-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o stream-analyzer-linux-arm64 .
```

Copy the one that matches the host to the Linux machine, with a
`channels.yaml`.

## Configure

```bash
cp channels.example.yaml channels.yaml
```

```yaml
data_dir: ./data
buffer: 3m
incident_storage_gb: 20
listen: 127.0.0.1:8765

channels:
  - name: channel1
    url: https://origin.example.com/live/channel1/index.m3u8
  - name: channel2
    url: https://origin.example.com/live/channel2/index.m3u8
    rendition: 1280x720
```

`data_dir` is where the buffer, the incidents, the CSV files and the log
go. The `-data DIR` flag overrides it.

`buffer` is how much of each channel is kept, which is also the pre-roll
every incident starts with. Each extra minute costs about 7.5 MB per Mbps
per channel.

`incident_storage_gb` caps the buffer and the incidents together. Past
it, the oldest closed incidents are deleted.

`listen` is the manual capture endpoint. It has to be a loopback address,
or `""` to turn it off.

A channel's `rendition` picks what to watch from a master playlist:
`highest` (the default), `lowest`, a 0-based index, a `WxH` resolution, or
a substring of the rendition's URI. A media playlist URL is watched as it
is. Channel names can use letters, digits, `.`, `_` and `-`, and show up
in file names and in the capture URL.

The top-level values above are the defaults. An unknown key or an
out-of-range value stops the monitor with an error that names the key.
Every setting is in [docs/reference.md](docs/reference.md).

## Run on macOS

```bash
caffeinate -i ./stream-analyzer -config channels.yaml
```

`caffeinate -i` keeps the Mac from idle-sleeping while the monitor runs;
on a laptop, keep it on power with the lid open. The monitor logs to the
terminal and to `data/stream-analyzer.log`. Ctrl-C stops it after it closes
any open incidents.

To run it unattended and restart it if it exits, save this as
`~/Library/LaunchAgents/com.example.stream-analyzer.plist`, with your paths:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.stream-analyzer</string>
  <key>WorkingDirectory</key>
  <string>/Users/you/stream-analyzer</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/caffeinate</string>
    <string>-i</string>
    <string>/Users/you/stream-analyzer/stream-analyzer</string>
    <string>-config</string>
    <string>channels.yaml</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>KeepAlive</key>
  <true/>
  <key>ExitTimeOut</key>
  <integer>60</integer>
</dict>
</plist>
```

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.stream-analyzer.plist   # start
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.example.stream-analyzer.plist     # stop
```

`ExitTimeOut` gives the monitor 60 s to close open incidents after
`bootout`, longer than the 30 s an ffmpeg run may take. launchd starts it
with a short `PATH`: include the folder that `command -v ffmpeg` prints,
or set `blackdetect.ffmpeg` to the full path.

## Run on Linux

Install ffmpeg and the CA certificates:

```bash
sudo apt install ffmpeg ca-certificates     # Debian, Ubuntu
sudo dnf install ffmpeg ca-certificates     # Fedora, RHEL family
```

With dnf, the `ffmpeg` package comes from RPM Fusion, which has to be
enabled first; Fedora's own `ffmpeg-free` may not decode H.264, which
black detection needs. This prints an `h264` line when ffmpeg can decode
it:

```bash
ffmpeg -hide_banner -decoders | grep h264
```

Run the monitor in a terminal, or in tmux so it outlives the SSH session:

```bash
./stream-analyzer -config channels.yaml
tmux new -s stream-analyzer './stream-analyzer -config channels.yaml'
```

Detach from tmux with Ctrl-b d; `tmux attach -t stream-analyzer` brings it
back.

For a systemd service, give it a user and a folder (on an ARM host, copy
the arm64 binary instead):

```bash
sudo useradd --system --home-dir /opt/stream-analyzer --shell /usr/sbin/nologin stream-analyzer
sudo mkdir -p /opt/stream-analyzer
sudo cp stream-analyzer-linux-amd64 /opt/stream-analyzer/stream-analyzer
sudo cp channels.yaml /opt/stream-analyzer/
sudo chown -R stream-analyzer:stream-analyzer /opt/stream-analyzer
```

Save this as `/etc/systemd/system/stream-analyzer.service`:

```ini
[Unit]
Description=stream-analyzer
Wants=network-online.target
After=network-online.target

[Service]
User=stream-analyzer
Group=stream-analyzer
WorkingDirectory=/opt/stream-analyzer
ExecStart=/opt/stream-analyzer/stream-analyzer -config channels.yaml
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now stream-analyzer
journalctl -u stream-analyzer -f
```

`systemctl stop` sends SIGTERM, and the monitor closes open incidents
before it exits. That can wait for an ffmpeg run of up to
`blackdetect.timeout` (30 s), so `TimeoutStopSec` is 60. The data folder
is `/opt/stream-analyzer/data`.

## Manual capture

```bash
curl -X POST 'http://127.0.0.1:8765/capture?channel=channel1'
```

```json
{
  "dir": "data/incidents/20261001T120000Z_channel1",
  "incident": "20261001T120000Z_channel1",
  "status": "opened"
}
```

A capture opens an incident for the channel with the buffer as pre-roll
and 60 s of post-roll, or joins the one already open (`"status":
"merged"`). The endpoint listens only on localhost, so it can be reached
only from the same machine, and it refuses requests that come from a web
page. An unknown channel gets a 404 that lists the configured names.

## Triggers and thresholds

Every value below is the default in the code; `channels.example.yaml`
changes none of them. A rule without a threshold shows `on`.

### Faults that open an incident

| Name | What it means | Default | Config key |
|---|---|---|---|
| `video_dts_gap` | Video jumps at a segment boundary: the first frame's DTS is off by the threshold or more from one frame after the previous segment's last. Also that much video missing inside a segment, or video that disappears. | 500 ms | `checks.video_gap_fault_ms` |
| `video_pts_gap` | At a boundary, presentation jumps the threshold or more further than decoding does (the reorder delay changed). | 500 ms | `checks.video_gap_fault_ms` |
| `audio_pts_gap` | An audio hole or overlap of more than the threshold, at a boundary or between two audio packets inside a segment, or audio that disappears. | 1.5 AAC frames (32 ms at 48 kHz) | `checks.audio_gap_fault_frames` |
| `audio_coverage` | A segment's audio ends more than the threshold before its video, against the channel's usual A/V offset. | 1.5 AAC frames | `checks.audio_gap_fault_frames` |
| `video_dts_not_increasing` | A frame's DTS is not later than the one before it. | on | none |
| `video_pts_error` | A frame is due for display before it is decoded (PTS earlier than DTS). | on | none |
| `pts_behind_pcr` | A frame's PTS is not ahead of the PCR, the stream clock, at its first packet. | on | none |
| `pts_pcr_jump` | PTS minus PCR changes by more than the threshold between two frames, inside a segment or across a boundary. | 500 ms | `checks.pcr_jump_ms` |
| `av_offset` | A segment's A/V start offset is more than the threshold from the channel's baseline, the median of its first 5 segments. A new offset that holds for 10 segments becomes the baseline. | 100 ms | `checks.av_offset_ms`, `checks.av_baseline_segments`, `checks.av_rebaseline_after` |
| `duration_mismatch` | A segment's media duration differs from its EXTINF by more than the threshold. | 10 % | `checks.duration_tolerance_pct` |
| `black_video` | A black run of the threshold or longer on screen, joined across segments. ffmpeg's `pix_th` (0.10) and `pic_th` (0.98) decide what is black. | 1.0 s | `blackdetect.trigger_min` |
| `invalid_segment` | A 200 response that is not usable MPEG-TS, such as an HTML error page. | on | none |
| `ts_corruption` | Bytes that are not TS packets, or packets flagged `transport_error_indicator`, had to be skipped. | on | none |
| `discontinuity` | A new segment carries `EXT-X-DISCONTINUITY`. | on | none |
| `playlist_violation` | The playlist breaks a rule for live playlists: listed segments renumbered, a segment number skipped, an entry rewritten after it was listed, `EXT-X-DISCONTINUITY-SEQUENCE` changed without cause, or the window jumped past segments it never listed. | on | none |
| `media_sequence_backward` | `EXT-X-MEDIA-SEQUENCE` is lower than in the previous fetch. | on | none |
| `stall` | No new segment for the threshold times the target duration (21 s with 7 s segments). | 3 target durations | `stall_target_durations` |
| `stall_ended` | A stall that outlasted its own incident ended; records how long it lasted. | on | none |
| `stream_ended` | The playlist carries `EXT-X-ENDLIST`. No stall is raised while it does. | on | none |
| `unavailable` | A segment the playlist still lists is refused (4xx or 5xx) twice, 1 s apart. | on | none |
| `manual` | A request to the capture endpoint. | on | `listen` |

### Events that are only logged

They go to `data/events.csv`, with the segment's SCTE-35 context, and
open no incident. `health.csv` counts the frame gaps and the segments with
re-timed audio.

| Name | What it means | Default | Config key |
|---|---|---|---|
| `video_gap` | Video at a boundary is off by more than 10 ms but less than 0.5 s. | 10 to 500 ms | `checks.continuity_ms`, `checks.video_gap_fault_ms` |
| `presentation_gap` | Presentation at a boundary is off by more than 10 ms but less than 0.5 s beyond what decoding explains. | 10 to 500 ms | `checks.continuity_ms`, `checks.video_gap_fault_ms` |
| `frame_gap` | Inside a segment, a DTS step longer than 1.25 frames: skipped frame slots, under 0.5 s in all. | 1.25 frames | none |
| `frame_overlap` | Inside a segment, a DTS step shorter than 0.75 frames. | 0.75 frames | none |
| `duplicate_pts` | Two frames in a segment share a PTS. | on | none |
| `odd_length` | A segment's frame count differs from the channel's usual count, the most common of its last 50 segments (judged once it has 5). | 50 segments | none |
| `audio_gap` | Audio at a boundary starts more than 10 ms but at most 1.5 AAC frames from where the previous segment's audio ended. | 10 to 32 ms | `checks.continuity_ms`, `checks.audio_gap_fault_frames` |
| `audio_retimed` | Inside a segment, audio timestamps step more than 10 ticks (0.11 ms) away from the audio frames' duration, up to 1.5 AAC frames. | 10 ticks | none |
| `gap_tagged` | The playlist marks a segment `EXT-X-GAP`; it is not fetched. | on | none |
| short black run | A black run of at least `d` that never reaches `trigger_min`. Kept in the segment's JSON and counted as `short_black_runs` in `health.csv`, not in `events.csv`. | 0.1 s | `blackdetect.d` |

### Origin notes

Some origins break playlist rules routinely. These are logged, written to
`data/origin_notes.csv`, and listed in the `report.json` of the incident
whose evidence covers them, but they open no incident.

| Name | What it means | Default | Config key |
|---|---|---|---|
| `target_duration_changed` | `EXT-X-TARGETDURATION` changed; some origins raise it while a long segment is listed. Counted as `target_duration_changes` in `health.csv`. | on | none |
| `discontinuity_tag_dropped` | `EXT-X-DISCONTINUITY` was dropped from the first entry and the playlist sends no `EXT-X-DISCONTINUITY-SEQUENCE`. The discontinuity is not reported twice. | on | none |
| `first_entry_cue_rewritten` | The first entry gained cue tags, such as `CUE-OUT` turning into `CUE-OUT-CONT` at the head of an ad break. | on | none |

### Health warnings

Each channel writes a health line every minute, to the log and to
`data/health.csv`, and a last one when the monitor stops. The line is
`WARN` when any of these holds for the interval:

| Name | What it means | Default | Config key |
|---|---|---|---|
| `segments` | No segment arrived (not checked on the line written at shutdown). | 1 minute interval | `health_interval` |
| `segment_errors` | A segment fetch failed or was refused. | more than 0 | none |
| `playlist_errors` | A playlist fetch failed, was refused or did not parse. | more than 0 | none |
| `stalled` | The channel is stalled. | 3 target durations | `stall_target_durations` |
| `monitor_gaps` | Segments were never analyzed: the monitor's own fetch failed, or they left the playlist first. | more than 0 | none |
| `write_errors` | An evidence file could not be written. | more than 0 | none |
| `blackdetect_errors` | ffmpeg failed on a segment. | more than 0 | none |
| `queue_drops` | A segment was dropped because the channel's download queue (64) was full. | more than 0 | none |
| `resolve_errors` | The channel URL gave no usable playlist. | more than 0 | none |
| `panics` | The monitor recovered from an internal error. | more than 0 | none |
| `free_gb` | The data folder's disk has less free space than the threshold. Opening an incident then also logs an error. | 5 GB | `min_free_gb` |

### Incident timing

| Name | What it means | Default | Config key |
|---|---|---|---|
| Pre-roll | The buffer copied into an incident when it opens. | 3 minutes | `buffer` |
| Post-roll | Recording continues this long after the last fault. | 60 s | `post_roll` |
| Merge window | A fault this soon after the last one joins the open incident. | 60 s | `merge_window` |
| Maximum length | The incident closes at this age. A fault type still firing then is held back on that channel until it has been quiet for the merge window. | 10 minutes | `max_incident` |
| Manual capture | A capture gets a full post-roll, but never runs past the maximum length plus one post-roll. | 60 s | `post_roll` |
| Storage cap | The buffer and the incidents together. The oldest closed incidents are deleted first, checked after each close and every minute. | 20 GB | `incident_storage_gb` |

## Output

```
data/
  buffer/<channel>/                 the last 3 minutes of segments and playlists
  incidents/<UTC time>_<channel>/   one folder per incident
  incidents/incidents.csv           one row per closed incident
  health.csv                        one row per channel per minute
  events.csv                        irregular segments that are not faults
  scte35.csv                        every segment with an ad-signaling tag
  origin_notes.csv                  origin notes
  stream-analyzer.log               the log
  stream-analyzer.lock              held while a monitor runs
```

An incident folder holds:

```
report.json    faults with their timestamp values, verdicts from the other renditions,
               every segment's record, and the size and SHA-256 of every file
segments/      seg_<N>.ts as served, and seg_<N>.json: fetch, headers, timing summary, faults
playlists/     every playlist fetch: the .m3u8 and a .json with its response headers
renditions/<index>_<WxH>_<bandwidth>/   the same segments and playlists from each other rendition
```

`N` is the origin's segment number when the URI carries one
(`...-seq=N.ts`), otherwise the playlist position. For each fault,
`report.json` says whether each other rendition shows it too; a fault
reproduced in every rendition comes from upstream of the packager. All
times, in every file and log line, are UTC.

## Report and reanalyze

```bash
./stream-analyzer report -from 2026-10-01T00:00:00Z -to 2026-10-02T00:00:00Z
```

`report` summarizes a window from the CSV files, per channel: segments
monitored, ad breaks and their cadence, where irregular segments fall
relative to the splices, and the faults and incidents. It only reads the
data folder, so it can run next to the monitor. Times are UTC unless they
carry an offset, and `-channel NAME` limits it to one channel.

```bash
./stream-analyzer reanalyze -config channels.yaml data/incidents/20261001T120000Z_channel1
```

`reanalyze` runs the current checks again on a saved incident and writes
`report.v2.json` next to `report.json`, changing nothing else in the
folder. It is for re-reading old incidents after a threshold change or an
upgrade.

## Limitations

Black is the only picture problem it detects. A slate, a frozen picture
or silent audio passes as normal content.

It handles plain MPEG-TS segments only. A playlist with encryption
(`EXT-X-KEY`), fMP4 (`EXT-X-MAP`) or byte ranges (`EXT-X-BYTERANGE`) is
refused; low-latency parts are ignored and the full segments checked.

It watches one rendition per channel all the time. The others are fetched
only while an incident is open, for the segments its faults are on.
Alternate audio renditions (`EXT-X-MEDIA`) are not checked.

Only one monitor can use a data folder at a time; give a second one its
own `-data`.

The log and the CSV files are not rotated.

## Troubleshooting

Every fetch fails with `connection reset by peer`. Some CDNs reset the TLS
handshake when the client offers TLS 1.3, as Go's client does, instead of
falling back to 1.2. That is why `tls_max_version` defaults to `"1.2"`.
For an origin that needs TLS 1.3, set `tls_max_version: "1.3"`.

`x509: certificate signed by unknown authority` on Linux means the host
has no CA certificates. Install `ca-certificates`.

`data directory ./data is in use by another stream-analyzer (pid 1234)`:
another monitor has the data folder. Stop it, or give this one another
folder with `-data`. The lock goes away when that process exits, even
after a crash, so there is no lock file to delete.

The monitor exits at start with this:

```
stream-analyzer: blackdetect needs ffmpeg: exec: "ffmpeg": executable file not found in $PATH
```

Install ffmpeg, or set `blackdetect.ffmpeg` to its full path, since
launchd and systemd start with a short `PATH`. Setting
`blackdetect.enabled: false` runs without black detection.

Health lines turn `WARN` with a low `free_gb` when the data folder's disk
has less than `min_free_gb` (5 GB) free. The storage cap deletes old
closed incidents but never the buffer or an open incident, so lower
`incident_storage_gb` or `buffer`, or free up space.
