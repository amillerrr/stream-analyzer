# stream-analyzer reference

The details behind the README: the command line, every setting, how each
check works, and the formats of the files the monitor writes.

- [Command line](#command-line)
- [Settings](#settings)
- [What happens per channel](#what-happens-per-channel)
- [Checks](#checks)
- [Playlist checks and origin notes](#playlist-checks-and-origin-notes)
- [Irregular segments (events.csv)](#irregular-segments-eventscsv)
- [SCTE-35 cues (scte35.csv)](#scte-35-cues-scte35csv)
- [Incidents](#incidents)
- [Manual capture](#manual-capture)
- [Health lines (health.csv)](#health-lines-healthcsv)
- [stream-analyzer report](#stream-analyzer-report)
- [stream-analyzer reanalyze](#stream-analyzer-reanalyze)
- [Stream notes](#stream-notes)
- [Limitations in detail](#limitations-in-detail)
- [Tests](#tests)
- [Code layout](#code-layout)

## Command line

| flag | meaning |
|---|---|
| `-config channels.yaml` | config file |
| `-data DIR` | data directory (overrides `data_dir`) |
| `-listen ADDR` | manual-trigger address (overrides `listen`; loopback only) |
| `-channels a,b` | monitor only these channels from the config |
| `-debug` | debug logging |

`stream-analyzer report` and `stream-analyzer reanalyze` are subcommands; their
flags come after the subcommand name.

The monitor is a single binary built from the Go standard library.
Segments are downloaded as raw bytes over HTTP/1.1. ffmpeg never records:
it is only run read-only, for `blackdetect` on the saved files.

`caffeinate -i` stops a Mac from idle-sleeping while the monitor runs;
`caffeinate -s` also prevents system sleep on AC power.

The monitor logs to the terminal and to `data/stream-analyzer.log`, every
time in UTC: each line's own, any time in its values, and lines from Go's
standard library (such as the capture listener's errors), which go
through the same logger. It appends every health line to
`data/health.csv`. Stop it with Ctrl-C,
SIGTERM or by closing the terminal (SIGHUP): open incidents are closed
(reason `shutdown`) and indexed before it exits. A closed terminal or a
broken pipe on stderr doesn't stop it: the log file still gets every line,
and a crash's stack goes to the log file too.

Only one monitor can use a data directory at a time.
- While it runs, the monitor holds a lock on the data directory itself and
  on `data/stream-analyzer.lock`, which holds its pid. Deleting the lock file
  doesn't release the lock.
- A second monitor on the same directory exits at once with an error that
  names that pid. Two monitors would double every CSV row, and the second
  would mark the first one's open incidents as interrupted.
- The kernel releases the lock when the process ends, even after a crash,
  so there is never a stale lock to clear.
- `stream-analyzer report` and `reanalyze` don't take the lock and can run
  alongside.
- The lock needs flock, so it applies on macOS, Linux and the BSDs.
- On a filesystem without flock support, such as some network mounts, the
  monitor warns and runs without the lock.

## Settings

Every setting except `channels` is optional.

| setting | default | meaning |
|---|---|---|
| `data_dir` | `./data` | runtime data; required |
| `listen` | `127.0.0.1:8765` | manual trigger: a loopback address, or `""` for none |
| `log_file` | `stream-analyzer.log` | inside `data_dir` (it can't leave it or name one of the monitor's own files); an absolute path; or `""` = stderr only |
| `user_agent` | `stream-analyzer/1.0` | sent on every request; no control characters |
| `tls_max_version` | `"1.2"` | the highest TLS version offered, `"1.2"` or `"1.3"` (see [Stream notes](#stream-notes)) |
| `rendition` | `highest` | for master playlists: `highest`, `lowest`, a 0-based index, `WxH`, or a URI substring. `highest` and `lowest` skip audio-only variants (`CODECS` with no video codec and no `RESOLUTION`); with nothing else, the channel doesn't resolve. Channels can override it. |
| `buffer` | `3m` | rolling evidence per channel (10 s to 1 h) |
| `post_roll` | `60s` | keep recording after the last fault |
| `merge_window` | `60s` | faults this close together share one incident |
| `max_incident` | `10m` | longest incident; at least `post_roll` |
| `incident_storage_gb` | `20` | cap for the buffer and `data/incidents` together (1 GB = 10^9 bytes); at least 1 |
| `min_free_gb` | `5` | free space the data directory's disk should keep; under it every health line warns (`0` = don't check) |
| `health_interval` | `1m` | health line per channel |
| `stall_target_durations` | `3` | no new segment for this many target durations is a stall (1.5 to 100) |
| `checks.continuity_ms` | `10` | a boundary step this far from one frame (video) or from where the audio ended is logged as an event |
| `checks.video_gap_fault_ms` | `500` | a video gap or overlap this large or larger is a fault; smaller is an event |
| `checks.audio_gap_fault_frames` | `1.5` | an audio hole or overlap of more than this many AAC frames (1.5: 32 ms at 48 kHz) is a fault; smaller is an event |
| `checks.pcr_jump_ms` | `500` | largest allowed PTS-PCR change between frames |
| `checks.av_offset_ms` | `100` | allowed A/V start offset deviation from the baseline |
| `checks.av_baseline_segments` | `5` | segments whose median is the baseline (0 = no A/V offset check) |
| `checks.av_rebaseline_after` | `10` | a sustained new offset becomes the baseline after this many segments (0 = never) |
| `checks.duration_tolerance_pct` | `10` | real duration vs EXTINF |
| `blackdetect.enabled` | `true` | run ffmpeg blackdetect on every segment |
| `blackdetect.d` / `pix_th` / `pic_th` | `0.1` / `0.10` / `0.98` | ffmpeg blackdetect parameters: every run of `d` or longer is recorded. `pix_th` must be 0 to 0.2 and `pic_th` 0.8 to 1: outside those, ordinary pictures count as black (see the README) |
| `blackdetect.trigger_min` | `1.0` | the black frames (seconds of them) a run needs to open an incident by itself |
| `blackdetect.ffmpeg` | `ffmpeg` | the ffmpeg to run: a name on `PATH` or a full path. It is resolved once, at start, and that binary runs every check (see [ffmpeg at start](#ffmpeg-at-start)) |
| `blackdetect.allow_openh264` | `false` | start with an ffmpeg whose only H.264 decoder is OpenH264; the self-test must still pass |
| `blackdetect.workers` | `4` | concurrent ffmpeg processes across all channels |
| `blackdetect.timeout` | `30s` | per ffmpeg run |

Each channel has a `name`, a `url` and, optionally, a `rendition` that
overrides the top-level one, and a `color_range`: `limited` or `full` reads
the channel's luma in that range for black, whatever the stream signals;
left out, the stream's signal decides (untagged is limited). Use it for a
stream that is full range but doesn't say so, whose dark pictures would
otherwise count as black. Forcing a range decodes 10-bit video at 8 bits.
Every black run, `black_decode` and `black_video` fault records the
`pix_fmt` and `color_range` blackdetect judged the frames in, and
`report.json`'s `blackdetect.color_range` the channel's setting.

Unknown keys are errors, so a typo can't silently leave a default in place.
So is a value outside a range where the setting still does its job (a
`buffer` of 1 s, a storage cap of 0.5 GB, a `d` of 10^9). Channel names are
directory names: letters, digits, `.`, `_` and `-`, and they must differ in
more than case (on macOS's file system two names that differ only in case
are one directory). A channel URL can't carry `user:password@`, which would
be copied into every evidence file.

## ffmpeg at start

With `blackdetect.enabled` on, the monitor (and `reanalyze`) checks ffmpeg
before anything else, and doesn't start if a check fails:

1. `blackdetect.ffmpeg` is looked up once and resolved to the binary itself
   (symbolic links followed). Every ffmpeg run uses that path, so a change
   to `PATH`, a package upgrade or a moved link can't swap in another build
   unchecked; if the file goes away, runs fail as `blackdetect_errors`.
2. `ffmpeg -version` and `ffmpeg -decoders` give the version and the
   decoders ffmpeg uses for H.264 and HEVC (the first one listed for the
   codec that isn't experimental). A build with no H.264 decoder is
   refused, and so is one whose only H.264 decoder is OpenH264
   (`libopenh264`, as in EPEL's `ffmpeg-free`) unless
   `blackdetect.allow_openh264` is on: OpenH264 drops full-range video's
   range tag, so dark pictures count as black, mis-times frames on
   irregular segments, and can't decode interlaced, 10-bit or 4:2:2 video.
3. A self-test runs blackdetect exactly as the monitor does, with ffmpeg's
   default thresholds (0.10, 0.98), on three small H.264 clips built into
   the binary (made from ffmpeg's lavfi sources): limited range with
   B-frames and a black run across the 33-bit PTS wrap, beside dark frames
   that aren't black; full range with black frames and dark frames (luma 30
   of 255) that aren't; and a file with two video PIDs, the picture on one
   and black on the other, decoded once each. Every frame must be decoded,
   with no decode error, at the PTS the clip carries, and exactly the black
   frames must be black. Any other answer stops the monitor and names the
   clip and frame. With the OpenH264 builds tested (ffmpeg 7.1.5,
   `openh264` 2.3.1) the full-range clip's dark frames come out black, so
   `allow_openh264` alone doesn't get such a build past it.
4. The log gets one line with the resolved path, the version, both
   decoders and the self-test's result, and a warning when ffmpeg is older
   than 7.0 or its version can't be read (a build from git).

```
level=INFO msg=ffmpeg path=/usr/bin/ffmpeg configured=ffmpeg version=7.1.5 h264_decoder=h264 hevc_decoder=hevc
  self_test=passed self_test_decoder="h264 (native)" allow_openh264=false
```

## What happens per channel

1. It loads the channel URL. For a master playlist it picks one rendition to
   monitor and remembers the others. A response that is neither a master nor
   a media playlist (an error page, an empty 200) changes nothing. The load
   is retried with backoff, and again after every 5 consecutive playlist
   failures; each failure counts as `resolve_errors` in the health line.
2. It polls the media playlist twice per target duration (every 3.5 s for
   7 s segments, and never more often than every 250 ms or less often than
   every 10 s). Every fetch is saved as
   `playlist_<time>_msn<N>.m3u8`, with a `.json` beside it recording the URL,
   request and completion times, elapsed time, status, remote address, TLS
   version and the response headers as the server sent them (names as
   spelled, `Transfer-Encoding` included). A gzip body that is cut short or
   doesn't decode is kept as sent, as `.m3u8.gz`.
3. It checks the playlist against the previous one (see
   [Playlist checks](#playlist-checks-and-origin-notes)) and downloads each
   new segment raw, once per URI (`seg_<N>_<hash>.ts`), then checks it and
   writes `seg_<N>_<hash>.json`: playlist entry, fetch metadata, timing
   summary, black runs and faults. The first playlist queues its last 3 segments.
   - Any refusal (a 4xx or 5xx) is retried once after 1 s. If the retry works
     it's counted as `origin_errors_recovered` in the health line.
   - Our own network errors are retried twice with backoff (0.5 s, then 1 s).
   - Every attempt is kept: the failed ones in `failed_attempts`, each with
     its own headers, and each refusal's body as
     `seg_<N>_<hash>.attempt<K>.body`.
4. It keeps the last 3 minutes of all of this in `data/buffer/<channel>/`. It
   also keeps the 3 minutes before the last segment, so a long stall can't
   prune away what came before it. A segment's files, and a playlist
   fetch's, are pruned together. At start it prunes what an earlier run
   left, so an incident's pre-roll is never another run's.

A segment is known by its URI. Its number `N` (its record's `seq`) is the
packager's own, from a `seq=N` in the URI's file name (`...-seq=N.ts`), or
else its playlist position (`EXT-X-MEDIA-SEQUENCE` plus its index). A
`seq=` elsewhere in the URI, such as a token in the query, is not a
number. Numbers are each packager's convention, and HLS doesn't require
them to be unique: an inserted ad break may number its segments from 1, or
have none and take positions that content numbers also use. So a
segment's files are named by its number and the first 8 hex digits of its
URI's SHA-256 (`seg_<N>_<hash>`), its record in a report is its number and
URI, and a repeated number is no fault; a playlist that still lists the
newest segment queued is never taken for a stale copy, whatever its
numbers. Numbers order segments, count monitor gaps and match other
renditions. The position is kept as `msn`: an origin that renumbers its
playlist moves a listed segment to another position, so a position can
name two segments over time. A segment is compared with the entry the
playlist lists right before it (`prev_seq`, `prev_uri`); numbers the origin
skips are not monitor gaps. Other renditions' segments are matched by the
same number, and checked to start at the same media time (within 0.5 s).
`reanalyze` reads incidents saved with the earlier names, `seg_<N>.ts`, too.

Relative URIs resolve against the URL a playlist was actually served from,
after any redirects, as a player's would. The fetch metadata records it as
`final_url`.

## Checks

All timestamp arithmetic is modulo 2^33, so PTS, DTS and PCR wrap at 2^33
(about every 26.5 hours) without false faults. A segment is compared with
the one the playlist lists before it, when that one was analyzed.

One size rule applies at segment boundaries and inside segments: a video gap
or overlap of `video_gap_fault_ms` (0.5 s) or more, or an audio hole or
overlap of more than `audio_gap_fault_frames` AAC frames (1.5: 32 ms at
48 kHz, so one missing frame plus jitter is not), is a fault. Anything
smaller is an event (see
[Irregular segments](#irregular-segments-eventscsv)). The audio limit also
allows the 2 ticks a packager rounds by. A video frame is the channel's
nominal frame: the master playlist's `FRAME-RATE`, or else the most common
frame duration of its recent segments. A few black segments at half the
frame rate don't change it.

| fault | meaning |
|---|---|
| `video_dts_gap` | at a boundary, the first video DTS is `video_gap_fault_ms` or more from one frame after the previous segment's last DTS, or the segment lost its video; inside a segment, a DTS step that leaves that much video missing |
| `video_pts_gap` | at a boundary, the first picture is due `video_gap_fault_ms` or more further from one frame after the previous segment's last picture than the DTS step explains (a changed reorder delay) |
| `video_pts_error` | a picture is due before it is decoded (PTS before DTS) |
| `video_dts_not_increasing` | a video DTS within the segment isn't greater than the one before |
| `audio_pts_gap` | an audio hole or overlap of more than `audio_gap_fault_frames` AAC frames (32 ms): between the previous segment's audio end (last PES + its ADTS frames) and this segment's first audio PTS, or between two PES inside a segment; or the audio disappeared |
| `audio_coverage` | the segment's audio ends more than `audio_gap_fault_frames` AAC frames (32 ms) before its video does (against the channel's A/V baseline). The next segment's audio starting late is the same hole, not a second fault. |
| `pts_behind_pcr` | a video PTS is not ahead of the last PCR at or before its PES |
| `pts_pcr_jump` | PTS−PCR changed by more than `pcr_jump_ms` between consecutive frames, within a segment or across the boundary |
| `av_offset` | the segment's A/V start offset (min video PTS − min audio PTS) is more than `av_offset_ms` from the channel baseline |
| `duration_mismatch` | real duration (video DTS span + one frame, or the audio span when there is no video) differs from EXTINF by more than `duration_tolerance_pct` |
| `black_video` | a black run with at least `trigger_min` (1 s) of black frames ffmpeg decoded, joined across segment boundaries. Time with no frame at all, and frames ffmpeg could not decode, are reported with it but never count (see below). |
| `undecoded_frames` | ffmpeg did not decode frames the TS parser found, after the first frame it did decode: the black check could not see them. Frames dropped before the first decodable one are not this fault (see below). |
| `invalid_segment` | a 200 response that isn't usable MPEG-TS, such as an HTML error page; its `Content-Type` and `Content-Encoding` are in the values |
| `ts_corruption` | bytes that were not TS packets (sync lost) or packets flagged `transport_error_indicator` were skipped; the counts are in the values |
| `discontinuity` | a new segment carries `EXT-X-DISCONTINUITY` |
| `playlist_violation` | the playlist broke a rule for live playlists; see [Playlist checks](#playlist-checks-and-origin-notes) |
| `stall` | no new segment for `stall_target_durations` × the target duration (21 s for 7 s segments) |
| `stall_ended` | the end of a stall that outlasted its own incident, with its length |
| `stream_ended` | the playlist carries `EXT-X-ENDLIST`: the origin ended the stream. No stall is raised while it does. |
| `unavailable` | a segment the playlist still lists (fetched again to check) is refused (any 4xx or 5xx), and still is on the retry 1 s later |
| `media_sequence_backward` | `EXT-X-MEDIA-SEQUENCE` is lower than in the previous playlist fetch |
| `manual` | `POST /capture` |

- **Discontinuities.** A tagged discontinuity is its own fault. The checks
  against the previous segment and the boundary PTS-PCR check are skipped
  across it. A segment's discontinuity number is the playlist's
  `EXT-X-DISCONTINUITY-SEQUENCE` plus the tags up to and including it; a
  change the playlist doesn't explain is a `playlist_violation`.
- **Monitor gaps** are not faults. A segment is a gap if it was never
  analyzed because of our own network error (no answer, or a 2xx whose body
  didn't arrive), because it left the playlist before we got it, or because
  the queue was full. The next segment is not compared across the hole. The
  gap is logged, counted in the health line, noted as `monitor_gap_before`
  in the segment's JSON and listed in any open incident. An `unavailable`
  segment is accounted for, so it is not a gap; nor are numbers the origin
  never listed.
- **Stalls.** The stall clock only counts playlist fetches the origin
  answered; our own network errors say nothing about the stream. A stall
  incident stays open while the stall lasts, up to `max_incident`. When new
  segments appear it gets a note with the stall's length and closes after
  the usual post-roll, so the resumption is recorded too. A stall that
  outlasted its incident gets a short incident of its own for its end
  (`stall_ended`).
- **Black runs.** ffmpeg (with `-copyts`) decodes only the video stream the
  TS parser analyzed (`-map 0:i:0x<PID>`): the first program's video PID
  with the lowest number. A segment with a second video stream or a second
  program can't have the other one judged instead, and a PID the file
  doesn't carry fails the run. ffmpeg prints every frame it decodes with
  its PTS in 90 kHz ticks and blackdetect's mark for it, and a run is placed
  on its frames' own PTS, each matched to the parser's frame with that PTS.
  So ffmpeg's rounding (before 7.0 it prints times to 6 significant digits,
  a frame off at large PTS) and its unwrapping of timestamps near the 33-bit
  wrap (negative times when a file starts in the last minute before it)
  don't move a run, and a run that lasts to the end of a segment is told
  from one that ends a frame before it.

  A run is the black frames ffmpeg output one after another, and it ends
  one frame after the last of them. A run's length is its black frames'
  time on screen (`black_frames_s`, each frame shown until the next, one
  frame at most): ffmpeg's own runs last until the next frame it decodes,
  so they also hold time with no frame at all and frames it could not
  decode, and neither is black. Each run also gives its time on screen
  (`duration_s`), the frames inside it that ffmpeg did not decode
  (`undecoded_frames`) and the time with no frame (`no_frame_s`), which the
  timing checks report on their own (`frame_gap`, `video_dts_gap`).

  A run that reaches the end of one segment and continues within half a
  frame at the start of the next, or after frames missing altogether for at
  most 0.5 s, counts as one run. Every run with `d` (0.1 s) or more of black
  frames is recorded in the segment's JSON and in the `black_runs` list of
  any report covering it; only a run with `trigger_min` (1 s) of black
  frames opens an incident by itself. Where an encoder drops frames during
  black, the time with no frame was 34 to 47 % of a run's time on screen.
- **Decoded frames.** Each segment's record has `black_decode`: the frames
  the parser found, the frames ffmpeg decoded, the frames dropped before
  the first one ffmpeg could decode (`leading_dropped`: expected for a
  segment decoded on its own that has an open GOP or doesn't start on an
  IDR), and the frames not decoded after that (`undecoded`, with where they
  are). Those raise `undecoded_frames`. Frames are missing only when
  ffmpeg output fewer frames than the parser found; pairing each decoded
  frame with a parser frame within half a frame of its PTS says which.
  ffmpeg re-times frames that share or jitter their PTS, so a frame with no
  decoded frame at its own PTS was not necessarily lost.
- **Not checked.** A segment's black check is incomplete when ffmpeg
  decoded no frame, or after its first frame left frames undecoded or
  logged decode errors (ffmpeg exits 0 unless more than 2/3 of the frames
  fail, so its exit status alone says little). Frames dropped before the
  first decodable one, and errors logged while ffmpeg looked for it, are
  expected and don't count. So does output ffmpeg printed that can't be
  read with certainty: a blackdetect line that isn't a black run, a time
  that isn't a finite number, a run that ends before it starts or whose
  duration isn't the time between its ends, a frame with no timestamp, or
  printed runs the frames' marks don't show (times are compared to the
  digits ffmpeg printed). Then nothing from that run of ffmpeg is used.
  `black_decode.not_checked` says why, the
  segment counts in `black_not_checked`, and black found in it is kept but
  marked `unconfirmed`, in the segment's runs, in `black_runs` and in a
  `black_video` fault. With `blackdetect.enabled` on, a segment with no
  video stream or no saved file is not checked either. Another
  rendition's segment that wasn't fully checked reproduces a black fault
  when it shows enough black frames, and is `inconclusive` when it
  doesn't.
- **PTS order.** Each segment's pictures are checked for PTS before DTS (a
  fault) and for two pictures due in one frame slot (`duplicate_pts`, an
  event). At a boundary, presentation is compared as well as decoding.
- **SCTE-35.** Ad-signaling tags in the playlist (`EXT-X-CUE-*`,
  `EXT-OATCLS-SCTE35`, `EXT-X-SCTE35`, `EXT-X-DATERANGE` with SCTE35
  attributes) are recorded per segment. Each fault lists the ones within two
  segments of it.
- **A/V baseline.** The baseline is the median of the first 5 segments. On
  the streams this was developed against, the start offset jitters between
  0 and −20 ms, within one AAC frame. This check still runs across
  discontinuities, since only the continuity checks are exempt.
- **TS parsing.** A PES without timestamps (allowed; one is required only
  every 700 ms) is counted: a video frame's DTS is placed between its
  neighbours', and audio continues the PES before it. A PES header split
  across packets is reassembled. Audio whose PES durations aren't known (not
  ADTS) isn't checked for continuity.

## Playlist checks and origin notes

Each playlist is compared with the previous one (RFC 8216 sections 6.2.1
and 6.2.2). A break is a `playlist_violation` fault on the playlist, with
the reason in its values and both playlists' files named:

| reason | meaning |
|---|---|
| `renumbered` | a listed segment moved to another position (the origin moved `EXT-X-MEDIA-SEQUENCE` further than the segments that left) |
| `skipped_number` | a new entry's `seq=N` is more than one past the entry before it, in the same directory: numbers that packager never used. A number that repeats or goes back, or one in another directory (an inserted ad), is not |
| `rewritten_entry` | an entry already listed changed its EXTINF or tags (other than the first entry's cue tags; see below), or a new URI is listed at a position (media sequence number) an old URI had, when the playlist wasn't renumbered |
| `discontinuity_sequence` | `EXT-X-DISCONTINUITY-SEQUENCE` changed by something other than the discontinuities that left the playlist |
| `discontinuity_tag_dropped` | `EXT-X-DISCONTINUITY` was removed from a listed segment while `EXT-X-DISCONTINUITY-SEQUENCE` is sent but not incremented |
| `window_jump` | the playlist moved past numbers it never listed, faster than the origin could have produced them (judged only when the two ends' numbers are one packager's) |

Some origins break these rules routinely. These are origin notes, not
faults:

| reason | meaning |
|---|---|
| `target_duration_changed` | `EXT-X-TARGETDURATION` changed. Some origins raise it while a segment longer than 7.5 s is listed, around black and filler segments. |
| `discontinuity_tag_dropped` | `EXT-X-DISCONTINUITY` was removed from a segment once it became the first entry, and `EXT-X-DISCONTINUITY-SEQUENCE` isn't sent, so the segment's discontinuity number changed. The discontinuity isn't reported a second time. |
| `first_entry_cue_rewritten` | the entry that became first gained cue tags: the origin re-tags the head of a break, `CUE-OUT` becoming `CUE-OUT-CONT`. The note names the tags it lost and gained. (Cue tags the first entry only loses, which happens at every break on some origins, aren't noted.) |

An origin note is logged once, and written as a row in
`data/origin_notes.csv` (`time_utc`, `channel`, `seq`, `uri`, `reason`,
`detail`). `health.csv` counts the target duration changes
(`target_duration_changes`). Each note is listed, with its values and both
playlists' files, in the `origin_notes` of the incident whose evidence
covers it: the one open when it was seen, or the next one to open within
the buffer window, where it is marked `pre_roll: true`. It opens no
incident.

A playlist that doesn't end with a newline, has no
`EXT-X-TARGETDURATION`, or has a URI line with whitespace, quotes or angle
brackets (an error page after `#EXTM3U`) is not a playlist: it counts as a
failed fetch, not as the origin's playlist. A target duration outside 1 to
30 s is logged and ignored, so it can't stop polling or stall detection.

## Irregular segments (events.csv)

Some irregularities are logged as events, not faults. Opening an incident
for each would fill the storage cap in hours: on the streams this was
developed against they show up in about 9 of 30 channel-minutes. Each event
is one row in `data/events.csv`, with these columns:

- `time_utc` (when the segment was fetched), `channel`, `seq`, `type`
- `video_frames`, `audio_frames` (AAC frames), `gap_ms`
- `scte35`: whether that segment or the one before it carries a SCTE-35
  tag in the playlist
- `scte35_tag`: the SCTE-35 tag type on the segment itself:
  - `OUT`: `#EXT-X-CUE-OUT`, the first segment of an ad break
  - `CONT`: `#EXT-X-CUE-OUT-CONT`, a segment inside one
  - `IN`: `#EXT-X-CUE-IN`, the first segment after it
  - `OTHER`: any other SCTE-35 tag
  - blank: no tag

  An `EXT-X-DATERANGE` with `SCTE35-OUT` or `SCTE35-IN` counts as `OUT` or
  `IN`. A segment signaling two positions, such as a break ending as the
  next begins, gets both joined with `+` (`IN+OUT`).
- `detail`

| type | meaning | `gap_ms` |
|---|---|---|
| `video_gap` | at a boundary, the first video DTS is more than `continuity_ms` but less than `video_gap_fault_ms` from one frame after the previous segment's last | the deviation (negative: an overlap) |
| `frame_gap` | a video DTS step inside the segment longer than 1.25 frames, leaving less than `video_gap_fault_ms` missing: skipped frame slots | the missing time |
| `frame_overlap` | video DTS steps inside the segment shorter than 0.75 frames | the time overlapped |
| `presentation_gap` | at a boundary, the first picture is due more than `continuity_ms` (but less than `video_gap_fault_ms`) further off than the DTS step explains | the difference |
| `duplicate_pts` | pictures in the segment share a PTS: two due in one frame slot | 0 (`detail` has the count) |
| `odd_length` | a video frame count unlike the channel's usual one, which is the most common count among its last 50 segments. Judged once 5 segments have been seen. | length difference (negative: short) |
| `audio_retimed` | audio PTS steps more than 10 ticks (0.11 ms) from the frames' duration, inside the segment | net re-timing (negative: squeezed) |
| `audio_gap` | audio starts more than `continuity_ms` from where the previous segment's ended, but within `audio_gap_fault_frames` AAC frames (32 ms) | the gap |
| `gap_tagged` | the playlist marks the segment `EXT-X-GAP`: it has no media, and it is not fetched | its EXTINF |

On the streams this was developed against, the audio PTS steps jitter by
up to about 2 ms (200 ticks), well past the 10-tick tolerance, so
`audio_retimed` is frequent; a real hole of more than 1.5 AAC frames is an
`audio_pts_gap` fault instead.

A segment can have several events. `health.csv` counts `frame_gaps` (skipped
frame slots) and `audio_retimed` (segments) per minute. Events and SCTE-35
cues are also logged at debug level (`-debug`).

## SCTE-35 cues (scte35.csv)

Every segment whose playlist entry carries a SCTE-35 tag gets a row,
irregular or not. The columns are:

- `time_utc` (when it was fetched), `channel`, `seq`
- `scte35_tag`, as in `events.csv`
- `extinf`, in seconds
- `tags`: the raw tag lines, joined with ` | `

Where every segment of an ad break is tagged, this is about a quarter of
all segments. `stream-analyzer report` reads it for the splice points.

## Incidents

A fault opens `data/incidents/<UTC time>_<channel>/`:

```
report.json
segments/     buffered + post-roll segments, their .json sidecars, and refused attempts' bodies
playlists/    every playlist fetch in the window, with headers (.json)
renditions/<index>_<WxH>_<bandwidth>/   the same segments and the playlists from each other rendition
```

Every file is a real copy, written to a temporary file and renamed into
place, so a file is either whole or not there, and no later write to the
buffer can change it.

- **Merging.** Faults on the same channel within 60 s of each other merge
  into one incident. It stays open, and keeps recording, until 60 s after the
  last fault. A segment being checked when the incident's time is up still
  joins it, with its sidecar, before it closes.
- **Other renditions.** For every fault on a segment, that segment and the
  entry listed before it are fetched from each other rendition, by the
  origin's number, while those still list them, then checked the same way,
  black runs included. Each rendition's playlist is fetched every poll while
  the incident is open; a stall is judged by those.
- **Rendition verdicts.** Each fault in `report.json` says per rendition, in
  `renditions`, with that rendition's own measurements in
  `rendition_values`:
  - `reproduced`: the same fault, at the same boundary, of about the same
    size (within `continuity_ms` or a tenth of the monitored fault's size,
    whichever is more; for `duration_mismatch`, 1 percentage point or a
    tenth).
  - `different`: the same type of fault but another size, or, for black, an
    overlapping run with less than `trigger_min` of black frames.
  - `not_reproduced`, with what the rendition measured there.
  - `inconclusive`: the check couldn't run there (ffmpeg failed or, for
    black, didn't fully decode the segment and found too little black; the bytes
    weren't TS), or the rendition lists another segment before this one.
  - `incomplete`: the segment before it is missing.
  - The segment's status: `pending`, `failed` (the last attempt failed;
    retried while listed), `expired` (it left the playlist first; the last
    fetch error is kept), `not_listed` (the rendition skips that number) or
    `mismatched` (its copy doesn't start at the monitored copy's media
    time).
  - The other fault types have their own rules:
    - `stall`: whether that rendition listed a newer segment while the
      monitored one was stalled (`not_reproduced`), listed nothing newer
      (`reproduced`), or wasn't fetched then (`pending`, then
      `inconclusive`).
    - `unavailable`: whether its origin refused the same segment.
    - Tagged `discontinuity`: whether it has the same tag.
    - Playlist-level faults: no verdict; the renditions' playlists are saved.

  A fault reproduced in every rendition is upstream of the packager.
- **report.json** holds:
  - the fault types and count, and the sequence numbers;
  - each fault with its segment's URI and its timestamp values (raw 33-bit
    ticks and milliseconds);
  - every black run in the covered segments (`black_runs`), with its black
    frames (`black_frames`, `black_frames_s`), undecoded frames and time
    with no frame (`no_frame_s`);
  - for each `black_video` fault, `values.thumbnails` names a strip in
    `thumbnails/`, `black_<N>_<hash>.png`: 160x90 tiles of the frame decoded
    before the run, its first, middle and last black frames, and the frame
    decoded after it, each taken by its PTS from the segment that holds it
    (one ffmpeg run, on a worker slot). The `.json` beside it gives each
    tile's role, segment file and PTS; a run that lasts to the end of its
    segment has no "after" tile yet (`continues`), and a tile whose segment
    file is gone is listed under `missing`. Reanalyze makes none;
  - `blackdetect`: how black was checked. The ffmpeg found at start (its
    resolved path, version, H.264 and HEVC decoders and the self-test's
    decoder), the decoders the listed segments went through
    (`decoders_used`), the filter graph and stream ffmpeg ran, the
    configured `d`, `pix_th`, `pic_th` and `trigger_min`, and the fixed
    values: the `d` given to ffmpeg (0), the 0.5 s join gap, the half-frame
    edge tolerance, the 29.97 fps fallback frame, the workers, the timeout
    and `allow_openh264`. `report.v2.json` has the reanalysis's;
  - the discontinuity and SCTE-35 tags present;
  - `origin_notes`: the origin notes seen while it was open, and in its
    pre-roll (`pre_roll: true`);
  - monitor gaps and notes (such as a stall ending);
  - every segment's record: its number, position, URI, the entry before it,
    its fetch and its timing summary;
  - the rendition results;
  - `files`: every file in the directory, with its size and SHA-256, once
    the incident has closed.
- **incidents.csv.** Each incident gets one row in
  `data/incidents/incidents.csv` (shared by all channels) when it closes,
  with the columns `id`, `channel`, `rendition`, `status`, `opened_utc`,
  `closed_utc`, `duration_s`, `fault_count`, `fault_types`, `first_fault`,
  `seq_first`, `seq_last`, `close_reason` and `dir`. An incident left open
  by a crash or kill is marked `interrupted` on the next start and indexed
  then, once.
- **Max duration.** `max_incident` caps a single incident at 10 minutes. If
  a fault type is still firing at the cap (seen at least twice in the last
  `merge_window`, say a channel stuck on black), that type is suppressed on
  that channel until it has been quiet for `merge_window`. It still counts
  in the health line as `suppressed`, and an incident that is open lists it.
  Other fault types keep opening incidents.
- **Storage cap.** The buffer and the incidents together are capped at 20
  GB. The cap is checked after every close and every minute, and the oldest
  closed incidents are deleted first; only directories named like an
  incident are. When the buffer and open incidents alone are over the cap,
  that is logged. Separately, every health line warns when the data
  directory's disk has less than `min_free_gb` free.

## Manual capture

`POST /capture?channel=NAME` opens an incident for the channel, capturing
the buffer, 60 s of post-roll and the other renditions of the latest
segment. If an incident is already open, the capture merges into it and
gets its full 60 s, even past `max_incident`, but never past `max_incident`
plus one post-roll: repeated captures can't hold an incident open forever.
The reply names the incident, with `status` `opened` or `merged`. A capture
that races the monitor's shutdown gets 503, and an unknown channel 404 with
the list of channel names.

The endpoint only listens on a loopback address (`listen` must be one, or
`""` to turn it off). A request whose `Host` isn't a loopback address (a
DNS-rebinding name), or that comes from a web page (it carries `Origin`, or
`Sec-Fetch-Site` other than `none`), is refused with 403 and logged.

## Health lines (health.csv)

Incidents are logged when they open, merge and close, as is every fault,
gap, fetch failure, stall end and refused capture. Each channel also gets a
health line every minute, plus a last one (marked `final=true`) for the part
of a minute before the monitor stops. A line is `WARN` if fetches failed,
the channel is stalled, nothing arrived (except in the final line), or
anything stopped the monitor collecting evidence or checking the stream:
a file it couldn't write, a blackdetect failure, a segment whose black
check was incomplete or skipped, a segment dropped from a full queue, a monitor gap, a channel URL it couldn't load, a recovered
panic, or free space under `min_free_gb`. The same values are appended to
`data/health.csv`:

```
level=INFO msg=health channel=channel1 rendition=3_1280x720_4500000 seq=1204 segments=10 mb=28.6
  playlists=17 playlist_errors=0 stale_playlists=0 segment_errors=0 origin_errors_recovered=0 monitor_gaps=0
  faults=0 suppressed=0 target_duration_changes=0 short_black_runs=0 leading_frames_dropped=0 frame_gaps=0
  audio_retimed=6 last_new_segment_age=1.2s av_offset_ms=-10.644 av_baseline_ms=-7.3 min_pts_pcr_ms=42.2
  min_dts_pcr_ms=8.9 write_errors=0 blackdetect_errors=0 black_not_checked=0 queue_drops=0 resolve_errors=0
  panics=0 last_processed_age=2.1s free_gb=812.4 incident=none
```

The timing columns are logged only; there is no alert on them.

- `min_pts_pcr_ms` and `min_dts_pcr_ms` are the smallest PTS−PCR and
  DTS−PCR over the minute, at each frame's first packet. A frame without a
  DTS uses its PTS. A mux that stamps the PCR at DTS − 8.9 ms shows 8.9 here
  on every line.
- `short_black_runs` counts the black runs that ended in the minute with at
  least `d` but less than `trigger_min` of black frames. A run split across
  segments is counted once, when it ends.
- `leading_frames_dropped` counts the frames ffmpeg dropped before the first
  one it could decode in each segment: an open GOP, or a segment that
  doesn't start on an IDR, decoded on its own. They are expected, are not
  black, and don't make the line a warning.
- `black_not_checked` counts the segments whose black check was incomplete:
  ffmpeg decoded no frame, or after its first frame it left frames
  undecoded or logged decode errors. It also counts segments with no check
  at all while `blackdetect.enabled` is on: no video stream, or a file that
  couldn't be saved. Any of them makes the line a warning. Black found in
  such a segment is still recorded, marked `unconfirmed`.
- `frame_gaps` and `audio_retimed` count the minute's skipped frame slots
  and segments with re-timed audio (see
  [Irregular segments](#irregular-segments-eventscsv)).
- `write_errors`, `blackdetect_errors`, `queue_drops`, `resolve_errors` and
  `panics` count what went wrong in the monitor itself; any of them makes the
  line a warning. `last_processed_age` is how long ago the worker finished
  its last segment; `free_gb` is the free space on the data directory's
  disk.

`data/health.csv` has one row per channel per minute, with these columns:

- `time_utc`, `channel`, `rendition`, `seq`
- `segments`, `mb`, `playlists`, `playlist_errors`, `stale_playlists`,
  `segment_errors`, `origin_errors_recovered`, `monitor_gaps`
- `faults`, `suppressed`, `target_duration_changes`, `short_black_runs`,
  `leading_frames_dropped`, `frame_gaps`, `audio_retimed`, `stalled`,
  `last_new_segment_age_s`
- `av_offset_ms`, `av_baseline_ms`, `min_pts_pcr_ms`, `min_dts_pcr_ms`
- `write_errors`, `blackdetect_errors`, `black_not_checked`, `queue_drops`, `resolve_errors`,
  `panics`, `last_processed_age_s`, `free_gb`, `incident`

If an existing `health.csv`, `events.csv` or `scte35.csv` has other columns
(from another version), it is renamed to `<name>.<UTC time>.csv` first, so
no file mixes two layouts. A row a crash cut short is ended before the next
one is written, so the two don't run together.

The log's times are UTC, like every file's. Durations the monitor waits
out (polls, post-rolls, stalls) run on the monotonic clock, so a clock
step or a sleep doesn't shorten or stretch them. A jump in the wall clock
(the Mac slept, or the time was set) is logged.

## stream-analyzer report

The report summarizes a run from the CSV files, per channel: where the
irregular segments fall relative to the ad breaks, the break cadence and
the faults. It only reads the data directory, so it can run alongside the
monitor.

```sh
./stream-analyzer report -from 2026-09-28T18:35:00Z -to 2026-09-28T19:21:00Z
./stream-analyzer report -from "2026-09-28 18:35" -to "2026-09-28 19:21" -channel channel1
```

| flag | meaning |
|---|---|
| `-from TIME`, `-to TIME` | the window (`-to` is exclusive), UTC unless the time has an offset: `2026-09-28T18:35:00Z`, `2026-09-28T11:35:00-07:00`, `"2026-09-28 18:35"` or `2026-09-28` |
| `-channel NAME` | report one channel |
| `-data DIR` | data directory; the default is `data_dir` from `-config` (`channels.yaml`) |

Each segment falls in one of three places:

- **At a splice:** a segment tagged `CUE-OUT` or `CUE-IN`, or the segment
  just before or after it. On the streams this was developed against, the
  irregular segments around each splice were all within that window.
- **In a break:** any other segment from `CUE-OUT` up to `CUE-IN`, or one
  tagged `CUE-OUT-CONT`.
- **In programming:** everywhere else.

For each channel the report prints:

- **`monitored`:** segments analyzed and monitor gaps, from `health.csv`.
  After a restart the segments a monitor fetched again are counted once.
- **`splice points`:** the `CUE-OUT` and `CUE-IN` segments fetched in the
  window, and how many have an irregular segment at the splice.
- **`breaks`:**
  - how many began in the window;
  - the median time between consecutive breaks, counted only across
    stretches the monitor saw in full. An interval spanning a restart or a
    monitor gap could hide a break, so it is left out. Segments after the
    last health line count as seen.
- **`break length`:**
  - **declared:** the length on the `CUE-OUT` tag, such as
    `#EXT-X-CUE-OUT:120.000`.
  - **from CUE-OUT to CUE-IN:** the sum of EXTINF between them. This
    includes any part of the `CUE-OUT` segment before the splice, so where
    the packager starts that segment before the splice it runs a few
    seconds over. A break that ended early shows as shorter. Fetch times
    aren't used, because a segment is listed only once it is complete. A
    break with an untagged segment inside it gets no EXTINF length.
- **`faults`:** the faults counted on the health lines in the window.
- **`incidents`:** every incident that overlaps the window, taken from
  `incidents.csv`, or from its `report.json` if it isn't indexed yet (for
  example, still open). One that opened before the window shows its date.
  A `report.json` still marked open after a crash stops counting once it
  hasn't been written for a while, and at most 11 minutes after it opened
  (the default `max_incident` plus `post_roll`).
- **A table of segments by place:**
  - all monitored segments come first, as the base rate to compare
    against;
  - then the irregular segments, first all together and then by type. A
    segment with several types counts once under each.
- **Every splice point:** the types of the irregular segments at offsets
  -1, 0 and +1 from it.

When the report covers more than one channel, a final `all channels`
section adds them up.

An example for one channel over 31 minutes. (Its one fault, a half-frame
+16.7 ms video DTS step on a `CUE-IN` segment, was recorded by an earlier
version; a step that small is a `video_gap` event now.)

```
stream-analyzer report: 2026-09-28 20:44:00 to 21:15:00 UTC, data ./data
  at a splice: within one segment of a CUE-OUT or CUE-IN segment
  in a break: from CUE-OUT up to CUE-IN otherwise; in programming: everywhere else

channel1
  monitored      292 segments, 0 monitor gaps
  splice points  3 CUE-OUT, 3 CUE-IN; 6 of 6 with an irregular segment
  breaks         3, every 7.7 min (median, range 7.6–7.8)
  break length   120 s declared; 122 s (median, range 120–125) from CUE-OUT to CUE-IN
  faults         1
  incidents      1
                 21:06:35  closed  video_dts_gap  1 fault  incidents/20260928T210635Z_channel1

                  segments     at a splice      in a break  in programming
  all monitored        292         18   6%         53  18%        221  76%
  any irregular         33         11  33%          0   0%         22  67%
  frame_gap              8          4  50%          0   0%          4  50%
  odd_length            10          6  60%          0   0%          4  40%
  audio_retimed         32         10  31%          0   0%         22  69%
  audio_gap              0

  splice points (UTC)
  20:49:10  CUE-OUT  seq 61772780   0: audio_retimed   +1: audio_retimed
  20:51:16  CUE-IN   seq 61772801  -1: frame_gap odd_length audio_retimed    0: frame_gap audio_retimed
  20:56:56  CUE-OUT  seq 61772858  -1: odd_length audio_retimed    0: odd_length audio_retimed
  20:58:56  CUE-IN   seq 61772878  -1: frame_gap odd_length audio_retimed    0: frame_gap audio_retimed
  21:04:31  CUE-OUT  seq 61772934   0: odd_length
  21:06:33  CUE-IN   seq 61772955  -1: odd_length audio_retimed    0: audio_retimed
```

Notes:

- Flags go after the subcommand: `stream-analyzer report -config … -from …`.
  `stream-analyzer -config … report …` is an error rather than a second
  monitor.
- `-to` is exclusive, and health lines are stamped to the second. To
  include the last health line of a run that has stopped, set `-to` at
  least a second after the stop, or later.
- Splice points come from `scte35.csv`. When `events.csv` has rows in the
  window from a version that didn't log cues, the report says so, and those
  segments count as in programming.
- Cues and irregular segments are read up to an hour outside the window,
  so segments near either end are placed by the break around them.
- Files moved aside when their columns changed (`<name>.<UTC time>.csv`)
  are read too, by column name.
- Each line is read on its own, with `\n` or `\r\n` endings. A line it
  can't read, such as a row a crash cut short, or a last line without its
  newline, is skipped and counted in a note, without affecting the lines
  after it.
- The base rate comes from each health line's `seq` and `segments`, so at
  each end of the window it covers whole health intervals.
- A 30-day window over ten channels takes about 4 s; a one-hour window
  takes well under a second.

## stream-analyzer reanalyze

```sh
./stream-analyzer reanalyze -config channels.yaml data/incidents/20260105T120000Z_channel1
```

This runs the current checks again on incidents already saved, with the
thresholds and blackdetect settings from `-config`, and writes the result to
`report.v2.json` in each incident directory. Nothing else there changes:
the playlists, segments and `report.json` stay as they are, and
`report.v2.json` lists them all, `report.json` included, with their SHA-256.
It prints one line per incident with the fault counts before and after.

- The monitored rendition's saved playlists are replayed in the order they
  were fetched, through the same code the monitor runs, and each segment
  they queue is checked from its saved file, found by its URI. Segments
  before the first saved one and after the last are skipped; a missing one
  in between is a monitor gap.
- It also reads incidents saved by versions that named segment files by
  playlist position. A renumbered playlist could make such a version
  download a segment twice: the second copy is noted, and not compared
  with the first. A file without a record takes its URI from the saved
  playlists.
- The other renditions are rebuilt from their saved playlists and segments
  and judged with the same rules. A segment a fault needs that the original
  run never fetched from a rendition is `not_saved`.
- Every fault found is listed, none suppressed. A fault found before the
  original incident opened, in its pre-roll, is marked `pre_roll: true`: a
  monitor running today's checks would have reported it then, in an
  incident of its own or one already open. The origin notes seen in the
  saved playlists are listed the same way.
- Manual captures are carried over. The notes say when it ran, the fault
  counts before and after, and anything it could not reanalyze.

## Stream notes

Observations from the streams this was developed against, which explain
some of the defaults.

- **TLS.** Some CDNs negotiate TLS 1.2 and reset the connection on any Go
  ClientHello that allows TLS 1.3, even with `CurvePreferences` set to
  X25519 only. LibreSSL offering 1.3 is downgraded to 1.2 cleanly, and
  forced to 1.3 it gets a proper alert, so the reset is specific to Go's
  hello. That is why `tls_max_version` defaults to `"1.2"`. Use `"1.3"`
  for origins that need it. Such origins speak HTTP/1.1; so does the
  monitor.
- **Ad breaks.** Each channel had a 120 s break about every 7.5 minutes,
  staggered across channels.
  - Splice points with a timing irregularity within one segment of them:
    111 of 119 in a 45-minute run, and 72 of 77 in a 30-minute one.
    Every `CUE-IN` had one, at the segment before it or its own. Counting
    `odd_length` on `CUE-OUT` segments that the packager shortened and
    declared correctly in their EXTINF would raise those to 119 of 119 and
    76 of 77.
  - Irregular segments inside a break away from its edges: none in either
    run.
  - The rest came during programming.
  - In the 30-minute run, 2 of 38 `CUE-IN` segments started with a
    half-frame (+16.7 ms) video DTS step.
- **Playlists.** The origin sometimes renumbered its playlist (moving
  `EXT-X-MEDIA-SEQUENCE` two ahead while one segment left) and skipped
  segment numbers, and it removed `EXT-X-DISCONTINUITY` from the first
  entry without `EXT-X-DISCONTINUITY-SEQUENCE`. It changed
  `EXT-X-TARGETDURATION` around every long black or filler segment (61
  times in 2.8 hours of saved incident playlists), but not during 15
  minutes of normal programming on ten channels. A monitor that numbers
  segments by playlist position downloads a segment twice after such a
  renumbering and reports a 2 to 6 s timestamp jump that isn't in the
  stream, which is why segments are known by their URI, not their
  position.
- **Black video.** At `d=0.1` blackdetect also finds the fades to black
  inside programming, such as trailer cuts of about 0.5 s. That's why only
  runs with `trigger_min` (1 s) of black frames open incidents; the short
  ones are still recorded. The encoder dropped frames during black, so
  black segments held fewer frames than usual, with holes of up to 2 s
  between them: that time with no frame is not counted as black.

## Limitations in detail

- **Segment formats.** Only whole, clear MPEG-TS segments are supported. A
  playlist with `EXT-X-KEY` (other than `METHOD=NONE`), `EXT-X-MAP` or
  `EXT-X-BYTERANGE` is refused, with the reason, as a failed fetch. So is a
  master playlist with `EXT-X-SESSION-KEY` (other than `METHOD=NONE`): it
  applies to every variant, and the channel URL then never resolves
  (`resolve_errors` in the health line).
  Low-latency parts (`EXT-X-PART`) are ignored, as a client that doesn't
  play low-latency may; the full segments are checked. `EXT-X-GAP` segments
  are not fetched (`gap_tagged`).
- **Field-coded video.** The black check compares the frames ffmpeg
  decoded with the parser's, which counts one frame per video PES. A
  field-coded (PAFF) H.264 stream that carries each field in a PES of its
  own would show half its frames as undecoded (`undecoded_frames`, not
  checked). None of the streams this was tested against is field-coded,
  and none could be generated to test it.
- **Alternate audio.** Only audio muxed into the segments is checked;
  `EXT-X-MEDIA` renditions are not fetched.
- **Log size.** The log is not rotated; it grows by a few MB a day.

## Tests

```sh
go vet ./...
go test -race ./...
go build ./...
```

The fixtures in `internal/tstest/testdata/*.ts` are small synthetic segments
with exact, hand-checkable timestamps:

| fixture | content |
|---|---|
| `cont_0..2` | three continuous segments |
| `wrap_0..1` | PTS/DTS and audio wrap inside segment 0; PCR wraps in segment 1 |
| `wrapb_0..1` | the wrap falls exactly on the segment boundary |
| `jump_1` | cont_1 moved 10 s later |
| `pcr_behind` | PTS behind PCR |
| `pcr_jump` | PTS-PCR offset jump |
| `dts_repeat` | repeated DTS |
| `avshift` | audio 200 ms late |

`internal/tstest` can also build segments shaped like production ones, and
the analysis tests use them: 29.97 fps with B-frames, 48 kHz AAC with
several frames per PES, and the PCR at DTS − 8.9 ms.

The tests cover:

- **analysis:**
  - PTS wrap, discontinuities with and without the tag, and the skipped
    segment.
  - The size rule at boundaries and inside segments, both thresholds
    configurable; audio coverage; PTS order, duplicate PTS and presentation
    at a boundary; the nominal frame and a sparse black segment.
  - Frame gaps, overlaps, odd lengths, audio re-timing, and audio gaps as
    events up to 1.5 AAC frames.
  - TS parsing: PES without timestamps, split PES headers, lost sync, TEI
    packets, non-AAC audio, and fuzzing.
- **hls:** the update rules (renumbering, skipped numbers, target duration,
  rewritten entries, discontinuity sequence and dropped tags); segment
  keys unique when numbers repeat, with no violation for a repeat;
  truncated and garbage playlists; unsupported tags, `EXT-X-SESSION-KEY`
  included; audio-only variants skipped by `highest` and `lowest`; the
  SCTE-35 tag types.
- **blackdetect**, against the ffmpeg on `PATH` and against fake or
  wrapped ffmpegs:
  - every decoded frame with its exact PTS and mark, only the given PID
    decoded, decode errors before and after the first frame, the decoder,
    pixel format and color range, and a forced range;
  - output that can't be read with certainty is an error: odd black lines,
    frames with no timestamp, runs the frames don't show; ffmpeg 5.1's
    coarse times and negative times near the wrap are read;
  - the version and decoder lists, the OpenH264 refusal, the binary
    resolved once, and the self-test, which passes on this ffmpeg and
    fails on a build that decodes the other video stream, loses the
    full-range tag or shifts timestamps.
- **monitor, end to end against a local HTTP origin:**
  - Segments known by the origin's number through a renumbering: each URI
    fetched once, no false jump, renditions compared at the same numbers.
  - A dropped connection becomes a monitor gap.
  - A listed segment that keeps answering 404, 403 or 410 is
    `unavailable`, with every attempt's headers and body kept.
  - A 503 or 429 cured by the retry is counted.
  - A segment that left the window is a gap.
  - A timestamp jump becomes an incident with evidence and cross-rendition
    verdicts, each with the rendition's own values.
  - A tagged discontinuity opens a `discontinuity` incident without timing
    faults; a dropped tag is an origin note.
  - A target duration change is an origin note, counted in `health.csv`
    and listed in the incident whose evidence covers it; so is the first
    entry's cue tags being rewritten.
  - An unexplained discontinuity-sequence change and a backward media
    sequence each open incidents.
  - A frozen playlist opens a `stall` incident that stays open until it
    resumes; `EXT-X-ENDLIST` is `stream_ended`, not a stall.
  - Black runs from real ffmpeg output joined across segments, and short
    runs at a boundary; black frames and missing frames apart.
  - The false-black cases (generated with ffmpeg at test time): another
    video stream in the file, a run near the 33-bit wrap, undecodable
    frames after black, a PTS jump after black, segments ffmpeg decodes
    nothing from or logs errors on, and thresholds that make pictures
    black. Only decoded black frames count; leading drops are expected;
    re-timed frames are not missing; skipped checks are not checked; the
    same rules in other renditions and in `reanalyze`; a channel's
    `color_range`; a thumbnail strip with every black fault; every
    `report.json` saying how black was checked.
  - Two segments with one number keep their own files and records; lower
    numbers after the newest segment queued are new, and a window jump
    needs one packager's numbers.
  - Irregular segments go to `events.csv` with their SCTE-35 flag, tag
    type, frame counts and gap sizes, and are counted in `health.csv`,
    without an incident.
  - Tagged segments go to `scte35.csv` with their tag type and EXTINF.
  - Failures make the health line a warning: unwritable files, ffmpeg
    failures, segments not fully checked, queue drops, unresolvable
    channels, panics, low disk; dropped leading frames don't.
  - The capture endpoint refuses non-loopback hosts and web pages.
  - Headers saved as sent, over plain HTTP and TLS; gzip playlists that
    don't decode kept as sent; segments never decoded.
  - A second monitor on a data directory in use refuses to start, even
    after the lock file is deleted. A read-only lock file still locks; a
    symlinked one isn't followed; a filesystem without flock gets a
    warning.
  - `reanalyze` finds what the monitor found on an incident it saved, and
    reads an incident saved with files named by position.
- **report**, from CSV files written the way the monitor writes them:
  - irregular segments placed at a splice, in a break or in programming,
    including a break that began before the window;
  - break cadence and EXTINF lengths, and cue patterns such as `IN+OUT`;
  - faults and incidents, including one not yet in `incidents.csv`, and a
    restart;
  - `-channel`, and older rotated files.
- **also covered:**
  - merging, the max-duration cap and suppression, and the storage cap;
  - incident edges: sidecars of segments checked as it closes;
  - stall-safe buffer pruning;
  - health minima, `health.csv`, including rotating an old layout;
  - crash recovery; config ranges and validation, the black thresholds'
    bounds and `color_range` included; the startup ffmpeg check, its
    refusal and its log line; every log time in UTC;
  - the `TestAudit*` tests, which reproduce specific failure cases.

Regenerate the fixtures after changing the generator:

```sh
go test ./internal/tstest -run TestFixturesUpToDate -update
```

The self-test's clips in `internal/blackdetect/selftest/` are made from
lavfi sources with libx264; make them again with:

```sh
go test ./internal/blackdetect -run TestSelfTestClips -update
```

Tests that need ffmpeg, or one of its encoders (`mpeg2video`, `mp2`,
`libx264`), skip without it.

## Code layout

```
main.go                 flags, config, logging, signals, report and reanalyze commands
channels.example.yaml   a starting config
internal/config         YAML config -> typed config, validation
internal/yamlite        the YAML subset parser (stdlib only)
internal/hls            master/media playlists, update rules, variant selection, SCTE-35 tags
internal/ts             TS packets, PES headers, PAT/PMT (from ts-validator), 33-bit clock math
internal/analysis       per-segment timing extraction and the checks
internal/blackdetect    ffmpeg blackdetect runner, startup checks, self-test and its clips
internal/monitor        polling, buffer, incidents, renditions, CSV, cap, HTTP trigger, health, reanalyze
internal/report         the report subcommand: splice points, irregular segments, breaks, faults
internal/tstest         synthetic TS builder and fixtures
docs/reference.md       this file
```
