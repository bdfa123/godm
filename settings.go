package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Settings are the daemon-wide choices the Settings dialog edits. They are
// kept in tasks.json beside the list they apply to.
type Settings struct {
	// KeepAwake stops Windows sleeping on its own while a download is running.
	KeepAwake bool `json:"keep_awake"`
	// SortByType puts a new download in a subfolder of the default folder
	// chosen by what it is. Off by default: nobody asked for it yet.
	SortByType bool `json:"sort_by_type"`
	// Folders names the subfolder for each kind in folderKinds. An empty name
	// keeps that kind in the default folder itself.
	Folders map[string]string `json:"folders"`
	// Language is what the manager page, the tray menu and the notifications are
	// written in: langAuto follows the operating system, else langEN or langZH.
	Language string `json:"language"`
}

func defaultSettings() Settings {
	return Settings{KeepAwake: true, Folders: defaultFolders(), Language: langAuto}
}

// clone copies the folder map, so that a snapshot taken under the lock can be
// written out after it is released without racing a later edit.
func (s Settings) clone() Settings {
	f := make(map[string]string, len(s.Folders))
	for k, v := range s.Folders {
		f[k] = v
	}
	s.Folders = f
	return s
}

// settingsUpdate is a request to change settings. A field left out stays as it
// is, so the dialog can send only what the user touched.
type settingsUpdate struct {
	KeepAwake *bool `json:"keep_awake"`
	// AfterAll arms or clears the one-shot action for when the downloads
	// finish. It is not part of Settings because it is not kept.
	AfterAll   *string `json:"after_all"`
	SortByType *bool   `json:"sort_by_type"`
	// Folders holds only the kinds being renamed.
	Folders map[string]string `json:"folders"`
	// Language is auto, en or zh.
	Language *string `json:"language"`
}

func (m *Manager) KeepAwake() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.KeepAwake
}

func (m *Manager) SortByType() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.SortByType
}

// UpdateSettings checks the whole request before applying any of it, so a bad
// value cannot leave the others half changed.
func (m *Manager) UpdateSettings(u settingsUpdate) error {
	folders, kinds := make(map[string]string, len(u.Folders)), defaultFolders()
	for key, name := range u.Folders {
		if _, known := kinds[key]; !known {
			return fmt.Errorf("unknown kind of file %q", key)
		}
		clean, err := cleanFolderName(name)
		if err != nil {
			return err
		}
		folders[key] = clean
	}
	if u.Language != nil && !validLang(*u.Language) {
		return fmt.Errorf("unknown language %q: use %s, %s or %s", *u.Language, langAuto, langEN, langZH)
	}

	m.mu.Lock()
	if u.AfterAll != nil {
		if err := m.checkAfterChoiceLocked(afterAction(*u.AfterAll)); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	if u.KeepAwake != nil {
		m.settings.KeepAwake = *u.KeepAwake
	}
	if u.SortByType != nil {
		m.settings.SortByType = *u.SortByType
	}
	for key, name := range folders {
		m.settings.Folders[key] = name
	}
	if u.Language != nil {
		m.settings.Language = *u.Language
	}
	if u.AfterAll != nil {
		// Choosing the same thing again changes nothing. Choosing something else
		// starts counting finished downloads afresh.
		if act := afterAction(*u.AfterAll); act != m.after {
			m.after, m.afterSeen = act, false
		}
	}
	m.syncAwakeLocked()
	m.mu.Unlock()
	m.dirty.Store(true)
	return nil
}

// settingsView is what GET and POST /api/settings answer with.
func (m *Manager) settingsView() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings.clone()
	return map[string]any{
		"ok":                true,
		"keep_awake":        s.KeepAwake,
		"can_keep_awake":    m.power.keepAwake != nil,
		"after_all":         string(m.after),
		"after_all_options": m.afterOptionsLocked(),
		"sort_by_type":      s.SortByType,
		"folders":           s.Folders,
		"out_dir":           m.outDir,
		// What was chosen, and what that comes to on this computer right now.
		"language":        cleanLang(s.Language),
		"language_active": resolveLang(s.Language),
	}
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var u settingsUpdate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&u); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.mgr.UpdateSettings(u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.mgr.settingsView())
}
