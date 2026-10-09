package main

import (
	"encoding/json"
	"net/http"
)

// Settings are the daemon-wide choices the Settings dialog edits. They are
// kept in tasks.json beside the list they apply to.
type Settings struct {
	// KeepAwake stops Windows sleeping on its own while a download is running.
	KeepAwake bool `json:"keep_awake"`
}

func defaultSettings() Settings {
	return Settings{KeepAwake: true}
}

// settingsUpdate is a request to change settings. A field left out stays as it
// is, so the dialog can send only what the user touched.
type settingsUpdate struct {
	KeepAwake *bool `json:"keep_awake"`
}

func (m *Manager) KeepAwake() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.KeepAwake
}

// UpdateSettings checks the whole request before applying any of it, so a bad
// value cannot leave the others half changed.
func (m *Manager) UpdateSettings(u settingsUpdate) error {
	m.mu.Lock()
	if u.KeepAwake != nil {
		m.settings.KeepAwake = *u.KeepAwake
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
	return map[string]any{
		"ok":             true,
		"keep_awake":     m.settings.KeepAwake,
		"can_keep_awake": m.power.keepAwake != nil,
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
