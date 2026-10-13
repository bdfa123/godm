package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const stateVersion = 1

// State is the sidecar written next to the partial file so a killed process can
// pick up exactly where each segment stopped.
type State struct {
	Version int `json:"version"`
	// Kind is empty for a byte-range download. A segment download (HLS) writes
	// its own shape into the same sidecar path, and loadState must not mistake
	// one for the other: they resume in completely different ways.
	Kind     string    `json:"kind,omitempty"`
	URL      string    `json:"url"`
	FinalURL string    `json:"final_url"`
	Size     int64     `json:"size"`
	ETag     string    `json:"etag"`
	Filename string    `json:"filename"`
	Segments []SegSnap `json:"segments"`
}

// SegSnap is the serializable view of a Segment.
type SegSnap struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

const stateExt = ".godm"

func statePath(target string) string { return target + stateExt }

// isStateName reports whether a file of this name would be read as another
// file's sidecar. A sidecar is taken as godm's word that the file beside it
// is its own unfinished work and may be written into, so no download may ever
// be saved under such a name: its contents are the server's. Windows ignores
// case in names and drops dots and spaces from their end, so "x.GODM" and
// "x.godm. " are both x's sidecar there.
func isStateName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(name, " .")), stateExt)
}

func loadState(target string) (*State, bool) {
	b, err := os.ReadFile(statePath(target))
	if err != nil {
		return nil, false
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.Version != stateVersion || s.Kind != "" {
		return nil, false
	}
	return &s, true
}

// saveState writes through a temp file so a crash mid-write can never leave a
// truncated state file that would silently restart the whole download.
func saveState(target string, s *State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(target), b)
}

// writeFileAtomic replaces a file in one step, so a reader either sees the old
// contents or the new ones and never a half-written mix.
func writeFileAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".godm-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		// On Windows this is what happens while anything has the target open
		// without sharing deletes, a scanner or an indexer, and a sidecar is
		// rewritten every 400 ms. The callers carry on without the write, so
		// nothing else would ever remove what is left here.
		os.Remove(name)
		return err
	}
	return nil
}

var tempNameRe = regexp.MustCompile(`^\.godm-[0-9]+$`)

// staleTempAge is how old a temp file of writeFileAtomic must be to be taken
// for one left behind: a live one exists for the instant between its creation
// and its rename.
const staleTempAge = time.Hour

// sweepTemps removes the temp files writeFileAtomic left in dir when the
// process was killed between creating one and renaming it.
func sweepTemps(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !tempNameRe.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) >= staleTempAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func clearState(target string) { os.Remove(statePath(target)) }

// reusable reports whether an existing state file describes the same bytes we
// are about to fetch. ETag is the strong signal; size is the weak fallback.
//
// A refresh deliberately comes from a different URL, and very often from a
// different CDN node whose ETag differs for the same bytes, so it is matched on
// size alone. That is the same trade-off IDM makes; the caller has already
// confirmed the user is re-requesting this particular file.
func (s *State) reusable(pr *ProbeResult, url string, refresh bool) bool {
	if s.Size != pr.Size || s.Size <= 0 || !pr.Resumable {
		return false
	}
	if !refresh {
		if s.URL != url {
			return false
		}
		if s.ETag != "" && pr.ETag != "" && s.ETag != pr.ETag {
			return false
		}
	}
	if len(s.Segments) == 0 {
		return false
	}
	for _, sg := range s.Segments {
		// End before Start is also how a range with no end is written, the one
		// range of a download that could not be resumed. This server can, and
		// there is no asking it for bytes "0 to -1".
		if sg.Start < 0 || sg.End < sg.Start || sg.End >= s.Size || sg.Done < 0 || sg.Start+sg.Done > sg.End+1 {
			return false
		}
	}
	return true
}
