package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const stateVersion = 1

// State is the sidecar written next to the partial file so a killed process can
// pick up exactly where each segment stopped.
type State struct {
	Version  int       `json:"version"`
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

func statePath(target string) string { return target + ".godm" }

func loadState(target string) (*State, bool) {
	b, err := os.ReadFile(statePath(target))
	if err != nil {
		return nil, false
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.Version != stateVersion {
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
	p := statePath(target)
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
	return os.Rename(name, p)
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
		if sg.Start < 0 || sg.End >= s.Size || sg.Done < 0 || sg.Start+sg.Done > sg.End+1 {
			return false
		}
	}
	return true
}
