package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultPort = 16801

// TaskState is the lifecycle of one job in the daemon.
type TaskState string

const (
	StateQueued  TaskState = "queued"
	StateRunning TaskState = "running"
	StateDone    TaskState = "done"
	StateError   TaskState = "error"
	StateStopped TaskState = "stopped"
)

// TaskView is what the UI and the extension see.
type TaskView struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Filename  string    `json:"filename"`
	Path      string    `json:"path"`
	State     TaskState `json:"state"`
	Error     string    `json:"error,omitempty"`
	Size      int64     `json:"size"`
	Received  int64     `json:"received"`
	Speed     int64     `json:"speed"`
	Conns     int       `json:"conns"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

type managedTask struct {
	mu       sync.Mutex
	view     TaskView
	cancel   context.CancelFunc
	lastRecv int64
	lastAt   time.Time
	speedEMA float64
}

func (t *managedTask) snapshot() TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view
}

// progress folds raw byte counts into an exponentially smoothed rate so the UI
// does not flicker between 0 and 40 MB/s on every tick.
func (t *managedTask) progress(received, total int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.lastAt.IsZero() {
		dt := now.Sub(t.lastAt).Seconds()
		if dt > 0.05 {
			inst := float64(received-t.lastRecv) / dt
			if t.speedEMA == 0 {
				t.speedEMA = inst
			} else {
				t.speedEMA = 0.7*t.speedEMA + 0.3*inst
			}
			t.lastRecv, t.lastAt = received, now
		}
	} else {
		t.lastRecv, t.lastAt = received, now
	}
	t.view.Received = received
	if total > 0 {
		t.view.Size = total
	}
	t.view.Speed = int64(t.speedEMA)
}

// Manager owns every task and caps how many run at once.
type Manager struct {
	mu     sync.Mutex
	tasks  map[string]*managedTask
	order  []string
	slots  chan struct{}
	outDir string
	seq    atomic.Uint64
}

func NewManager(outDir string, parallel int) *Manager {
	if parallel <= 0 {
		parallel = 3
	}
	return &Manager{
		tasks:  map[string]*managedTask{},
		slots:  make(chan struct{}, parallel),
		outDir: outDir,
	}
}

type jobRequest struct {
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	Headers     map[string]string `json:"headers"`
	Connections int               `json:"connections"`
	OutDir      string            `json:"out_dir"`
}

func (m *Manager) Add(req jobRequest) (string, error) {
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		return "", fmt.Errorf("only http and https URLs are supported")
	}
	id := fmt.Sprintf("t%d-%d", time.Now().UnixMilli(), m.seq.Add(1))
	outDir := req.OutDir
	if outDir == "" {
		outDir = m.outDir
	}

	ctx, cancel := context.WithCancel(context.Background())
	mt := &managedTask{
		view: TaskView{
			ID:        id,
			URL:       req.URL,
			Filename:  req.Filename,
			State:     StateQueued,
			Size:      -1,
			Conns:     req.Connections,
			StartedAt: time.Now(),
		},
		cancel: cancel,
	}

	m.mu.Lock()
	m.tasks[id] = mt
	m.order = append(m.order, id)
	m.mu.Unlock()

	go m.run(ctx, mt, req, outDir)
	return id, nil
}

func (m *Manager) run(ctx context.Context, mt *managedTask, req jobRequest, outDir string) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(mt, StateStopped, "cancelled before start", "")
		return
	}

	mt.mu.Lock()
	mt.view.State = StateRunning
	mt.view.StartedAt = time.Now()
	mt.mu.Unlock()

	res, err := Download(ctx, Options{
		URL:         req.URL,
		Headers:     req.Headers,
		OutDir:      outDir,
		Filename:    req.Filename,
		Connections: req.Connections,
		OnProgress:  mt.progress,
	})
	if err != nil {
		if ctx.Err() != nil {
			m.finish(mt, StateStopped, "stopped by user", "")
		} else {
			m.finish(mt, StateError, err.Error(), "")
		}
		return
	}
	mt.mu.Lock()
	mt.view.Size = res.Size
	mt.view.Received = res.Size
	mt.view.Conns = res.Segments
	mt.mu.Unlock()
	m.finish(mt, StateDone, "", res.Path)
}

func (m *Manager) finish(mt *managedTask, st TaskState, errMsg, path string) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.view.State = st
	mt.view.Error = errMsg
	mt.view.EndedAt = time.Now()
	mt.view.Speed = 0
	if path != "" {
		mt.view.Path = path
		mt.view.Filename = baseName(path)
	}
}

func (m *Manager) List() []TaskView {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	tasks := make([]*managedTask, 0, len(ids))
	for _, id := range ids {
		if t, ok := m.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	m.mu.Unlock()

	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	t.cancel()
	return true
}

func (m *Manager) Remove(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false
	}
	t.cancel()
	delete(m.tasks, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return true
}

// ---------- HTTP ----------

type server struct {
	mgr   *Manager
	token string
}

// RunDaemon binds loopback only and publishes its port so the native host and
// the UI can find it.
func RunDaemon(port int, outDir string, parallel int) error {
	token, err := loadOrCreateToken()
	if err != nil {
		return err
	}

	// Refuse to become a second daemon. Without this check a double-click would
	// start another instance, which loses the port race, falls back to an
	// ephemeral port, and then clobbers the port file the first one published.
	if c, err := newDaemonClient(); err == nil && c.ping() == nil {
		log.Printf("a godm daemon is already running at %s; nothing to do", c.base)
		return nil
	}

	s := &server{mgr: NewManager(outDir, parallel), token: token}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// The port is held by something that is not us: take an ephemeral one.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	if err := writePort(actual); err != nil {
		return err
	}
	// Only clear the port file if it still points at us; a later daemon may
	// have taken over, and deleting its entry would strand every client.
	defer func() {
		if readPort() == actual {
			os.Remove(portPath())
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/ping", s.guard(s.handlePing))
	mux.HandleFunc("/api/download", s.guard(s.handleDownload))
	mux.HandleFunc("/api/tasks", s.guard(s.handleTasks))
	mux.HandleFunc("/api/cancel", s.guard(s.handleCancel))
	mux.HandleFunc("/api/remove", s.guard(s.handleRemove))

	log.Printf("godm daemon listening on http://127.0.0.1:%d/  (downloads -> %s)", actual, outDir)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// guard enforces the bearer token and rejects cross-origin browser callers.
// A random web page cannot read our responses thanks to CORS, but it could
// still fire off requests, so the token is the real gate.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "chrome-extension://") ||
			strings.HasPrefix(origin, "moz-extension://") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		} else if origin != "" && !strings.HasPrefix(origin, "http://127.0.0.1:") &&
			!strings.HasPrefix(origin, "http://localhost:") {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *server) authorized(r *http.Request) bool {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		if subtleEqual(strings.TrimPrefix(v, "Bearer "), s.token) {
			return true
		}
	}
	return subtleEqual(r.URL.Query().Get("token"), s.token)
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "version": version, "pid": os.Getpid()})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.mgr.Add(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *server) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "tasks": s.mgr.List()})
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": s.mgr.Cancel(r.URL.Query().Get("id"))})
}

func (s *server) handleRemove(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": s.mgr.Remove(r.URL.Query().Get("id"))})
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The token is injected here so the page can call the API without asking
	// the user to paste anything.
	fmt.Fprint(w, strings.Replace(uiHTML, "__TOKEN__", s.token, 1))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) || len(b) == 0 {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
