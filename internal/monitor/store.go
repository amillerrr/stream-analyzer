package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Evidence subdirectories inside an incident.
const (
	kindSegments  = "segments"
	kindPlaylists = "playlists"
)

const tmpPrefix = ".tmp-"

// stamp formats a fetch time for file names: 20260928T155642.123Z.
func stamp(t time.Time) string { return t.UTC().Format("20060102T150405.000Z") }

// writeFileAtomic writes via a temp file and rename, so readers never see
// a partial file.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), tmpPrefix+"*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v, jsontext.WithIndent("  "), jsontext.AllowInvalidUTF8(true), json.Deterministic(true))
}

func writeJSON(path string, v any) error {
	b, err := marshalJSON(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// copyFile copies src to dst through a temporary file and a rename, so dst
// is either complete or not there; an existing dst is replaced.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Dir(dst), tmpPrefix+"*")
	if err != nil {
		return err
	}
	_, cerr := io.Copy(f, in)
	if err := errors.Join(cerr, f.Chmod(0o644), f.Close()); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), dst)
}

// pruneBuffer removes buffer files older than window, except those from
// the window before the last segment (lastSegment; zero if none yet).
// During a stall nothing new arrives, so without that exception the
// segments from before it would age out while it lasts. A segment's files
// (seg_N.ts and its sidecar seg_N.json, written after its check) and a
// playlist's go together, by the newer of their times, so an incident
// never gets one without the other.
func pruneBuffer(dir string, now time.Time, window time.Duration, lastSegment time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	newest := map[string]time.Time{} // by name without extension
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		base := fileGroup(e.Name())
		if mod := info.ModTime(); mod.After(newest[base]) {
			newest[base] = mod
		}
	}
	keepFrom := now.Add(-window)
	beforeLast := lastSegment.Add(-window)
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		mod, ok := newest[fileGroup(e.Name())]
		if !ok || !mod.Before(keepFrom) || !lastSegment.IsZero() && !mod.Before(beforeLast) && !mod.After(lastSegment) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fileGroup names the group a buffer file is pruned with: a segment's files
// (seg_N.ts, its sidecar seg_N.json, and seg_N.attemptK.body for refused
// attempts) share one, and so do a playlist fetch's (.m3u8 or .m3u8.gz,
// and .json).
func fileGroup(name string) string {
	if rest, ok := strings.CutPrefix(name, "seg_"); ok {
		n, _, _ := strings.Cut(rest, ".")
		return "seg_" + n
	}
	for _, ext := range []string{".m3u8.gz", ".m3u8", ".json"} {
		if base, ok := strings.CutSuffix(name, ext); ok {
			return base
		}
	}
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// copyBuffer copies every buffered file of a channel into an incident:
// segments and their sidecars under segments/, playlists under playlists/.
func copyBuffer(bufDir, incidentDir string) error {
	entries, err := os.ReadDir(bufDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, tmpPrefix) {
			continue
		}
		kind := kindPlaylists
		if strings.HasPrefix(name, "seg_") {
			kind = kindSegments
		}
		err := copyFile(filepath.Join(bufDir, name), filepath.Join(incidentDir, kind, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) { // pruned meanwhile
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// manifest lists every regular file under an incident directory, except
// the report it goes into (report.json, or reanalyze's report.v2.json) and
// unfinished temporary files, with its size and SHA-256, in path order.
func manifest(dir, report string) ([]EvidenceFile, error) {
	var out []EvidenceFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || !d.Type().IsRegular() || rel == report || strings.HasPrefix(d.Name(), tmpPrefix) {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, EvidenceFile{Path: filepath.ToSlash(rel), Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return out, err
}

// dirSize sums the sizes of the regular files under dir. It reports the
// first thing it could not read, and counts everything else. A directory
// that doesn't exist is empty.
func dirSize(dir string) (int64, error) {
	var n int64
	var first error
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			var info fs.FileInfo
			if info, err = d.Info(); err == nil {
				n += info.Size()
			}
		}
		if err != nil && first == nil && !errors.Is(err, fs.ErrNotExist) {
			first = err
		}
		return nil
	})
	return n, first
}
