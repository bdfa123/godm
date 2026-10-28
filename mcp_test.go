package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSession serves the tools against the daemon API of m, as a running godm
// answers it, and returns a client connected to them.
func mcpSession(t *testing.T, m *Manager) *mcp.ClientSession {
	t.Helper()
	s := &server{mgr: m, token: "tok"}
	srv := newTestServer(t, s.routes())
	return mcpConnect(t, &mcpTools{daemon: func() (*daemonClient, error) {
		return &daemonClient{base: srv.URL, token: "tok", http: &http.Client{Timeout: 10 * time.Second}}, nil
	}})
}

func mcpConnect(t *testing.T, tools *mcpTools) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientSide, serverSide := mcp.NewInMemoryTransports()
	if _, err := tools.server().Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// mcpCall calls a tool and returns the text it answered with, and whether it
// was an error. A call the protocol itself turned down counts as an error too.
func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String(), res.IsError
}

// mcpOK calls a tool that has to succeed, and decodes its answer into out.
func mcpOK(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) string {
	t.Helper()
	text, failed := mcpCall(t, cs, name, args)
	if failed {
		t.Fatalf("%s(%v) failed: %s", name, args, text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s answered %q, not JSON: %v", name, text, err)
		}
	}
	return text
}

func TestTheMCPToolsAreDescribedForClaude(t *testing.T) {
	cs := mcpSession(t, listManager(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]bool{"status": true, "list_tasks": true, "get_task": true, "get_settings": true, "inspect_stream": true, "ytdlp_status": true}
	destructive := map[string]bool{"remove_tasks": true, "set_after_all": true}
	want := []string{"status", "list_tasks", "get_task", "get_settings", "inspect_stream", "ytdlp_status", "add_downloads", "add_torrent_file",
		"pause", "resume", "start_list", "remove_tasks", "set_limits", "set_task_options", "update_settings", "set_after_all",
		"change_address", "play", "open_folder", "ytdlp_update"}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, name := range want {
		tool := got[name]
		if tool == nil {
			t.Errorf("there is no %s tool", name)
			continue
		}
		a := tool.Annotations
		if a == nil || a.Title == "" || tool.Description == "" {
			t.Errorf("%s has no title or description", name)
			continue
		}
		if a.ReadOnlyHint != reads[name] {
			t.Errorf("%s says read only %v, want %v", name, a.ReadOnlyHint, reads[name])
		}
		if !reads[name] && (a.DestructiveHint == nil || *a.DestructiveHint != destructive[name]) {
			t.Errorf("%s says destructive %v, want %v", name, a.DestructiveHint, destructive[name])
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("there are %d tools, want %d", len(res.Tools), len(want))
	}
	// What a struct tag cannot say is in the schema too.
	schema, _ := json.Marshal(got["list_tasks"].InputSchema)
	if !strings.Contains(string(schema), `"download_list"`) || !strings.Contains(string(schema), `"maximum":200`) {
		t.Errorf("list_tasks does not list its views or limit: %s", schema)
	}
}

// mcpAddOne adds one link that has to be added, and returns its id and what
// the tool answered.
func mcpAddOne(t *testing.T, cs *mcp.ClientSession, args map[string]any) (string, string) {
	t.Helper()
	var out struct {
		Added   int        `json:"added"`
		Results []mcpAdded `json:"results"`
	}
	text := mcpOK(t, cs, "add_downloads", args, &out)
	if out.Added != 1 || len(out.Results) != 1 || out.Results[0].ID == "" {
		t.Fatalf("add_downloads answered %s", text)
	}
	return out.Results[0].ID, text
}

// fileWithSecret serves a file whose link carries a signature, the way a file
// host's does, and returns the link.
func fileWithSecret(t *testing.T, payload []byte) string {
	t.Helper()
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	t.Cleanup(srv.Close)
	return srv.URL + "/film.bin?sig=SECRET-SIGNATURE"
}

func TestMCPAddsAndListsDownloads(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	payload := makePayload(512 << 10)
	link := fileWithSecret(t, payload)

	id, addText := mcpAddOne(t, cs, map[string]any{"urls": []string{link}})
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))

	var listed struct {
		Total int       `json:"total"`
		Tasks []mcpTask `json:"tasks"`
	}
	text := mcpOK(t, cs, "list_tasks", map[string]any{"view": "finished"}, &listed)
	if listed.Total != 1 || listed.Tasks[0].ID != id || listed.Tasks[0].Name != "film.bin" || listed.Tasks[0].State != "done" {
		t.Errorf("list_tasks answered %s", text)
	}
	details := mcpOK(t, cs, "get_task", map[string]any{"id": id}, nil)
	if !strings.Contains(details, `"link":"http://`) || !strings.Contains(details, "/film.bin") {
		t.Errorf("get_task does not show the link: %s", details)
	}
	var status struct {
		Downloads map[string]int `json:"downloads"`
	}
	statusText := mcpOK(t, cs, "status", nil, &status)
	if status.Downloads["finished"] != 1 {
		t.Errorf("status answered %s", statusText)
	}
	// The signature on the link is never shown.
	for _, out := range []string{text, details, statusText, addText} {
		if strings.Contains(out, "SECRET") {
			t.Fatalf("a tool showed the link's query: %s", out)
		}
	}
}

func TestMCPDrivesTheDownloadList(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	cs := mcpSession(t, m)
	_, link := listFile(t, makePayload(2<<20))

	id, _ := mcpAddOne(t, cs, map[string]any{"urls": []string{link}, "later": true, "connections": 1})
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))

	var listed struct {
		Tasks []mcpTask `json:"tasks"`
	}
	mcpOK(t, cs, "list_tasks", map[string]any{"view": "download_list"}, &listed)
	if len(listed.Tasks) != 1 || !listed.Tasks[0].OnList || listed.Tasks[0].State != "listed" {
		t.Fatalf("the download list shows %+v", listed.Tasks)
	}
	var started struct {
		Started int `json:"started"`
	}
	mcpOK(t, cs, "start_list", nil, &started)
	if started.Started != 1 {
		t.Fatalf("start_list started %d, want 1", started.Started)
	}
	waitFor(t, 10*time.Second, "the download to run", stateIs(m, id, StateRunning))
	mcpOK(t, cs, "pause", map[string]any{"ids": []string{id}}, nil)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	mcpOK(t, cs, "list_tasks", map[string]any{"view": "download_list"}, &listed)
	if len(listed.Tasks) != 1 || !listed.Tasks[0].FromList {
		t.Errorf("a download started from the list and paused is not shown with it: %+v", listed.Tasks)
	}
	mcpOK(t, cs, "resume", map[string]any{"ids": []string{id}}, nil)
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))

	v, _ := findTask(m, id)
	mcpOK(t, cs, "remove_tasks", map[string]any{"ids": []string{id}}, nil)
	if _, err := os.Stat(v.Path); err != nil {
		t.Fatalf("removing without delete_files took the file: %v", err)
	}
	if _, ok := findTask(m, id); ok {
		t.Fatal("the download is still there")
	}
}

func TestMCPRemovesFilesOnlyWhenAsked(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	id, _ := mcpAddOne(t, cs, map[string]any{"urls": []string{fileWithSecret(t, makePayload(64<<10))}})
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	mcpOK(t, cs, "remove_tasks", map[string]any{"ids": []string{id}, "delete_files": true}, nil)
	waitFor(t, 10*time.Second, "the file to go", func() bool {
		_, err := os.Stat(v.Path)
		return os.IsNotExist(err)
	})
}

func TestMCPChangesLimitsAndSettings(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	mcpOK(t, cs, "set_limits", map[string]any{"downloads_at_once": 5, "max_speed_mib_per_s": 1.5}, nil)
	if m.Limit() != 5 || m.SpeedLimit() != 3<<19 {
		t.Errorf("limits are %d at once, %d B/s; want 5 and 1.5 MiB/s", m.Limit(), m.SpeedLimit())
	}
	mcpOK(t, cs, "set_limits", map[string]any{"max_speed_mib_per_s": 0}, nil)
	if m.SpeedLimit() != 0 {
		t.Errorf("a limit of 0 left %d B/s", m.SpeedLimit())
	}

	dir := filepath.Join(t.TempDir(), "films")
	mcpOK(t, cs, "update_settings", map[string]any{
		"list_start_time": "7:05", "list_daily": true, "seed_policy": "keep", "download_dir": dir, "keep_awake": false,
	}, nil)
	if m.listAt() != "07:05" || !m.listDaily() || fmt.Sprint(m.SeedPolicy()) != "keep" || m.OutDir() != dir || m.KeepAwake() {
		t.Errorf("settings are list %q daily %v, seeding %v, folder %q, awake %v", m.listAt(), m.listDaily(), m.SeedPolicy(), m.OutDir(), m.KeepAwake())
	}
	var settings map[string]any
	mcpOK(t, cs, "get_settings", nil, &settings)
	if settings["seed_policy"] != "keep" || settings["list_at"] != "07:05" || settings["download_dir"] != dir {
		t.Errorf("get_settings answered %v", settings)
	}

	// What a failed download does next, and a failed download saying when it
	// is tried again.
	mcpOK(t, cs, "update_settings", map[string]any{"on_failure": "forever", "retry_every_minutes": 10, "retry_times": 4}, nil)
	if p, e, n := m.RetryChoice(); p != "forever" || e != 10 || n != 4 {
		t.Errorf("failures are %s every %d minutes, %d times; want forever, 10, 4", p, e, n)
	}
	mcpOK(t, cs, "get_settings", nil, &settings)
	if settings["on_failure"] != "forever" || settings["retry_every"] != float64(10) {
		t.Errorf("get_settings answered %v", settings)
	}
	f := taskIn(m, "t1-9", StateRunning)
	failRun(m, f, StateError, 0)
	var got struct {
		Download mcpTask `json:"download"`
	}
	mcpOK(t, cs, "get_task", map[string]any{"id": "t1-9"}, &got)
	if got.Download.RetryAt == "" || got.Download.State != "error" {
		t.Errorf("a failed download to be tried again shows as %+v", got.Download)
	}
}

// What happens when all downloads finish goes through, with a computer that
// can sleep, and is refused on one that cannot. Neither ever really sleeps:
// the power operations here only count.
func TestMCPChoosesWhatHappensWhenAllFinish(t *testing.T) {
	m := listManager(t)
	slept := 0
	m.mu.Lock()
	m.power.sleep = func() error { slept++; return nil }
	m.power.shutdown = nil
	m.mu.Unlock()
	cs := mcpSession(t, m)
	text := mcpOK(t, cs, "set_after_all", map[string]any{"action": "sleep"}, nil)
	if m.AfterAll() != "sleep" || !strings.Contains(text, "sleep") {
		t.Errorf("set_after_all(sleep) answered %s, and godm will %q", text, m.AfterAll())
	}
	if text, failed := mcpCall(t, cs, "set_after_all", map[string]any{"action": "shutdown"}); !failed || !strings.Contains(text, "cannot shut") {
		t.Errorf("set_after_all(shutdown) on a computer that cannot answered %q", text)
	}
	mcpOK(t, cs, "set_after_all", map[string]any{"action": "nothing"}, nil)
	if m.AfterAll() != "nothing" || slept != 0 {
		t.Errorf("after setting nothing godm will %q, and slept %d times", m.AfterAll(), slept)
	}
}

func TestMCPSetsOneDownloadsOptions(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	_, link := listFile(t, makePayload(2<<20))
	id, _ := mcpAddOne(t, cs, map[string]any{"urls": []string{link}, "connections": 1})
	mcpOK(t, cs, "set_task_options", map[string]any{"id": id, "max_speed_mib_per_s": 0.5, "connections": 3}, nil)
	v, _ := findTask(m, id)
	if v.SpeedLimit != 1<<19 || v.ConnsWanted != 3 {
		t.Errorf("the download is limited to %d B/s with %d connections, want 512 KiB/s and 3", v.SpeedLimit, v.ConnsWanted)
	}
	mcpOK(t, cs, "remove_tasks", map[string]any{"ids": []string{id}, "delete_files": true}, nil)
}

// Several links are added one by one, and each says how it went.
func TestMCPAddsSeveralLinksAndSaysHowEachWent(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	good := fileWithSecret(t, makePayload(64<<10))
	var out struct {
		Added   int        `json:"added"`
		Results []mcpAdded `json:"results"`
	}
	text := mcpOK(t, cs, "add_downloads", map[string]any{"urls": []string{good, "ftp://files.test/a.bin?token=SECRET"}}, &out)
	if out.Added != 1 || len(out.Results) != 2 || out.Results[0].ID == "" || out.Results[1].ID != "" || out.Results[1].Error == "" {
		t.Fatalf("add_downloads answered %s", text)
	}
	if strings.Contains(text, "SECRET") {
		t.Errorf("add_downloads showed a link's query: %s", text)
	}
	waitFor(t, 20*time.Second, "the download", stateIs(m, out.Results[0].ID, StateDone))
}

func TestMCPAddsATorrentFileFromThisComputer(t *testing.T) {
	m := newBTManager(t)
	cs := mcpSession(t, m)
	mi := makeTorrent(t, t.TempDir(), "Some Film", 16<<10, tfile{data: makePayload(64 << 10)})
	path := filepath.Join(t.TempDir(), "Some Film.torrent")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var added struct {
		ID string `json:"id"`
	}
	mcpOK(t, cs, "add_torrent_file", map[string]any{"path": path, "later": true}, &added)
	v, ok := findTask(m, added.ID)
	if !ok || v.Kind != "bt" || !v.Later {
		t.Fatalf("the torrent is %+v, want a torrent on the download list", v)
	}
	details := mcpOK(t, cs, "get_task", map[string]any{"id": added.ID}, nil)
	if !strings.Contains(details, `"peers"`) || !strings.Contains(details, `"kind":"bt"`) {
		t.Errorf("get_task for a torrent answered %s", details)
	}
	// Nothing but a .torrent on this computer is read.
	for _, bad := range []string{filepath.Join(t.TempDir(), "notes.txt"), "Some Film.torrent", `\\files.test\share\Some Film.torrent`} {
		if text, failed := mcpCall(t, cs, "add_torrent_file", map[string]any{"path": bad}); !failed || !strings.Contains(text, "full path of a .torrent file on this computer") {
			t.Errorf("add_torrent_file(%q) answered %q", bad, text)
		}
	}
}

// A folder is a full path on this computer, outside the system's folders and
// those of programs, where a download could be made to run.
func TestMCPOnlySavesInFoldersItShould(t *testing.T) {
	m := listManager(t)
	cs := mcpSession(t, m)
	link := fileWithSecret(t, makePayload(64<<10))
	startup := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Startup`)
	for _, folder := range []string{"Films", `\\files.test\share`, "//files.test/share", startup, os.Getenv("ProgramData"), os.Getenv("SystemRoot")} {
		if folder == "" {
			continue
		}
		if text, failed := mcpCall(t, cs, "add_downloads", map[string]any{"urls": []string{link}, "folder": folder}); !failed {
			t.Errorf("add_downloads saved in %q: %s", folder, text)
		}
		if text, failed := mcpCall(t, cs, "update_settings", map[string]any{"download_dir": folder}); !failed {
			t.Errorf("update_settings made %q the download folder: %s", folder, text)
		}
	}
	if len(m.List()) != 0 {
		t.Fatalf("a refused folder still got a download: %+v", m.List())
	}
	// A folder of the user's own is fine, the temporary folder included.
	ok := filepath.Join(t.TempDir(), "Films")
	id, _ := mcpAddOne(t, cs, map[string]any{"urls": []string{link}, "folder": ok})
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); filepath.Dir(v.Path) != ok {
		t.Errorf("saved in %s, want %s", filepath.Dir(v.Path), ok)
	}
}

// What a server or godm says when something fails can quote a signed link;
// the tools show it without its query, as they show any link.
func TestMCPErrorsDoNotShowALinksQuery(t *testing.T) {
	v := TaskView{ID: "t1-1", Filename: "film.bin", State: StateError,
		Error: `probe gave up after 5 retries: probe: Get "https://dl.files.test/download/?fid=1&sign=SECRET": dial tcp: lookup dl.files.test: no such host`}
	b := briefTask(v, 0)
	if strings.Contains(b.Error, "SECRET") || !strings.Contains(b.Error, "https://dl.files.test/download/") {
		t.Errorf("a download's error is shown as %q", b.Error)
	}
	if got := scrubLinks("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=udp%3A%2F%2Ftracker.test&dn=x and more"); got != "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567 and more" {
		t.Errorf("a magnet link in an error is shown as %q", got)
	}

	// And an error a tool returns: here the daemon turning a link down in
	// words that quote it.
	daemon := http.NewServeMux()
	daemon.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) })
	daemon.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `fetching the torrent file: Get "https://tracker.test/dl.php?id=1&passkey=SECRET": 403 Forbidden`, http.StatusBadRequest)
	})
	daemon.HandleFunc("/api/address", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"error":"probe: Get \"https://dl.files.test/new?sign=SECRET\": dial tcp: no such host"}`))
	})
	srv := newTestServer(t, daemon)
	cs := mcpConnect(t, &mcpTools{daemon: func() (*daemonClient, error) {
		return &daemonClient{base: srv.URL, token: "tok", http: &http.Client{Timeout: 5 * time.Second}}, nil
	}})
	text, failed := mcpCall(t, cs, "add_downloads", map[string]any{"urls": []string{"https://tracker.test/dl.php?id=1&passkey=SECRET"}})
	if !failed || strings.Contains(text, "SECRET") || !strings.Contains(text, "https://tracker.test/dl.php") {
		t.Errorf("a refused link is reported as %q (error %v)", text, failed)
	}
	text, failed = mcpCall(t, cs, "change_address", map[string]any{"id": "t1-1", "url": "https://dl.files.test/new?sign=SECRET"})
	if !failed || strings.Contains(text, "SECRET") || !strings.Contains(text, "https://dl.files.test/new") {
		t.Errorf("a link godm could not use is reported as %q (error %v)", text, failed)
	}
}

func TestMCPNumbersAtTheirEdges(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1e-9: "1", 1.5: "1572864", 1e30: strconv.FormatInt(maxMiBPerSecond<<20, 10)} {
		if got := mibPerSecond(v); got != want {
			t.Errorf("mibPerSecond(%g) = %s, want %s", v, got, want)
		}
	}
	// A download crawling at a byte a second has no time left worth giving,
	// and none that overflows.
	b := briefTask(TaskView{State: StateRunning, Size: 80 << 30, Received: 1, Speed: 1}, 0)
	if b.Left != "" || b.Speed == "" {
		t.Errorf("at 1 B/s the time left is %q", b.Left)
	}
}

func TestMCPSaysWhatWentWrong(t *testing.T) {
	cs := mcpSession(t, listManager(t))
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_task", map[string]any{"id": "t1-1"}, "no download with id"},
		{"add_downloads", map[string]any{"urls": []string{"https://a.test/1", "https://a.test/2"}, "filename": "x.bin"}, "single link"},
		{"add_downloads", map[string]any{"urls": []string{"ftp://a.test/1"}}, ""},
		{"set_limits", map[string]any{}, "give downloads_at_once"},
		{"pause", map[string]any{}, "either ids or all"},
		{"list_tasks", map[string]any{"view": "everything"}, ""},
		{"remove_tasks", map[string]any{"ids": nil}, ""},
		{"pause", map[string]any{"ids": []string{}}, ""},
		{"update_settings", map[string]any{"list_start_time": "25:00"}, ""},
		{"update_settings", map[string]any{"on_failure": "explode"}, ""},
		{"update_settings", map[string]any{"retry_every_minutes": 0}, ""},
		{"update_settings", map[string]any{"retry_times": 101}, ""},
		{"change_address", map[string]any{"id": "t1-1", "url": "https://a.test/new"}, ""},
	}
	for _, c := range cases {
		text, failed := mcpCall(t, cs, c.tool, c.args)
		if !failed || !strings.Contains(text, c.want) {
			t.Errorf("%s(%v) answered %q (error %v), want an error saying %q", c.tool, c.args, text, failed, c.want)
		}
	}
	// An id that is not there is said to be not there, not done.
	text := mcpOK(t, cs, "pause", map[string]any{"ids": []string{"t1-1"}}, nil)
	if !strings.Contains(text, "not done") {
		t.Errorf("pausing a download that is not there answered %s", text)
	}
}

func TestMCPSaysWhenGodmIsNotRunning(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	cs := mcpConnect(t, &mcpTools{daemon: func() (*daemonClient, error) {
		return &daemonClient{base: gone.URL, token: "tok", http: &http.Client{Timeout: time.Second}}, nil
	}})
	for _, tool := range []string{"status", "list_tasks", "start_list"} {
		if text, failed := mcpCall(t, cs, tool, map[string]any{}); !failed || !strings.Contains(text, "godm is not running") {
			t.Errorf("%s with no godm answered %q", tool, text)
		}
	}
	noFiles := mcpConnect(t, &mcpTools{daemon: func() (*daemonClient, error) { return nil, fmt.Errorf("no port file") }})
	if text, failed := mcpCall(t, noFiles, "status", map[string]any{}); !failed || !strings.Contains(text, "cannot start it") {
		t.Errorf("status with no port file answered %q", text)
	}

	// A godm that answers but not to this token is running; opening it again
	// would not help, and the tools do not say it would.
	s := &server{mgr: listManager(t), token: "the-real-one"}
	srv := newTestServer(t, s.routes())
	stale := mcpConnect(t, &mcpTools{daemon: func() (*daemonClient, error) {
		return &daemonClient{base: srv.URL, token: "a-stale-copy", http: &http.Client{Timeout: time.Second}}, nil
	}})
	if text, failed := mcpCall(t, stale, "status", map[string]any{}); !failed || strings.Contains(text, "not running") || !strings.Contains(text, "turned the request down") {
		t.Errorf("status with the wrong token answered %q", text)
	}
}
