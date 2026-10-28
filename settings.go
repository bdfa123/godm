package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	// DownloadDir is the folder a new download goes to when it names none, as
	// chosen in the Settings dialog. Empty means the folder godm was started
	// with: the -o flag, GODM_DOWNLOAD_DIR, or the Downloads folder.
	DownloadDir string `json:"download_dir,omitempty"`
	// ListAt is the time of day, "15:04", at which the download list starts by
	// itself, empty for never; ListDaily makes that every day rather than
	// once. See downloadlist.go.
	ListAt    string `json:"list_at,omitempty"`
	ListDaily bool   `json:"list_daily,omitempty"`
	// OnFailure is what a download that fails does next, a failPolicy;
	// RetryEvery is the minutes between tries again and RetryTimes how many
	// there are under failRetry. See onfailure.go.
	OnFailure  string `json:"on_failure"`
	RetryEvery int    `json:"retry_every"`
	RetryTimes int    `json:"retry_times"`
}

func defaultSettings() Settings {
	return Settings{
		KeepAwake: true, Folders: defaultFolders(), Language: langAuto,
		OnFailure: string(failRetry), RetryEvery: defaultRetryEvery, RetryTimes: defaultRetryTimes,
	}
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
	// DownloadDir is a full path, or empty to go back to the folder godm was
	// started with.
	DownloadDir *string `json:"download_dir"`
	// ListAt is "15:04", or empty to stop starting the list by the clock.
	ListAt    *string `json:"list_at"`
	ListDaily *bool   `json:"list_daily"`
	// OnFailure is give_up, retry or forever; RetryEvery is in minutes.
	OnFailure  *string `json:"on_failure"`
	RetryEvery *int    `json:"retry_every"`
	RetryTimes *int    `json:"retry_times"`
}

// defaultDirLocked is the folder a new download goes to when it names none.
func (m *Manager) defaultDirLocked() string {
	if m.settings.DownloadDir != "" {
		return m.settings.DownloadDir
	}
	return m.outDir
}

// checkDownloadDir makes sure a folder chosen as the default can take
// downloads before it becomes the default: a full path, made if it is not
// there, and somewhere a file can be written. Finding out at the next
// download, which may be a click in the browser hours later, would be too
// late to say so. It returns the path cleaned, or "" for going back to the
// folder godm was started with.
func checkDownloadDir(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("the download folder must be a full path, like D:\\Downloads, not %q", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot use %s as the download folder: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".godm-write-check-*")
	if err != nil {
		return "", fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return dir, nil
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
	if u.OnFailure != nil {
		if err := checkFailPolicy(*u.OnFailure); err != nil {
			return err
		}
	}
	if u.RetryEvery != nil {
		if err := checkRetryEvery(*u.RetryEvery); err != nil {
			return err
		}
	}
	if u.RetryTimes != nil {
		if err := checkRetryTimes(*u.RetryTimes); err != nil {
			return err
		}
	}
	var listAt string
	if u.ListAt != nil {
		var err error
		if listAt, err = checkListAt(*u.ListAt); err != nil {
			return err
		}
	}
	// Checked outside the lock: it touches the disk, which can be a slow one.
	var dir string
	if u.DownloadDir != nil {
		var err error
		if dir, err = checkDownloadDir(*u.DownloadDir); err != nil {
			return err
		}
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
	if u.DownloadDir != nil {
		// The folder godm was started with is kept as no choice at all, so
		// that it goes on following -o or GODM_DOWNLOAD_DIR.
		if strings.EqualFold(dir, filepath.Clean(m.outDir)) {
			dir = ""
		}
		m.settings.DownloadDir = dir
	}
	if u.ListAt != nil {
		m.settings.ListAt = listAt
	}
	if u.ListDaily != nil {
		m.settings.ListDaily = *u.ListDaily
	}
	if u.ListAt != nil || u.ListDaily != nil {
		m.planListLocked(time.Now())
	}
	// A new choice for failures starts the downloads failed now on it afresh.
	// Saving the dialog with it as it was changes nothing.
	dropped := false
	if was := m.settings; u.OnFailure != nil || u.RetryEvery != nil || u.RetryTimes != nil {
		if u.OnFailure != nil {
			m.settings.OnFailure = *u.OnFailure
		}
		if u.RetryEvery != nil {
			m.settings.RetryEvery = *u.RetryEvery
		}
		if u.RetryTimes != nil {
			m.settings.RetryTimes = *u.RetryTimes
		}
		if m.settings.OnFailure != was.OnFailure || m.settings.RetryEvery != was.RetryEvery || m.settings.RetryTimes != was.RetryTimes {
			m.failing.Store(m.settings.retryChoice())
			dropped = m.replanRetriesLocked(failPolicy(was.OnFailure), time.Now())
		}
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
	if dropped {
		// Downloads that were to be tried again no longer hold the action
		// back. A change of setting finishes nothing, so it goes ahead only
		// if a download has finished since it was armed.
		m.checkAfterAll(false)
	}
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
		// Where a new download goes, the folder chosen for that if any, and
		// the one godm was started with, which an empty choice goes back to.
		"out_dir":      m.defaultDirLocked(),
		"download_dir": s.DownloadDir,
		"started_dir":  m.outDir,
		// When the download list starts by itself, and next does.
		"list_at":    s.ListAt,
		"list_daily": s.ListDaily,
		"list_next":  m.listNext,
		// What a failed download does next; see onfailure.go.
		"on_failure":  s.OnFailure,
		"retry_every": s.RetryEvery,
		"retry_times": s.RetryTimes,
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

func (m *Manager) listAt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.ListAt
}

func (m *Manager) listDaily() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.ListDaily
}
