package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// settingsAPI serves the settings endpoint behind the same guard as the real
// daemon and answers a request with the status and the decoded body.
func settingsAPI(t *testing.T, m *Manager) func(method, body, token string) (int, map[string]any) {
	t.Helper()
	s := &server{mgr: m, token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/settings", s.guard(s.handleSettings))
	srv := newTestServer(t, mux)
	return func(method, body, token string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+"/api/settings", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		json.Unmarshal(raw, &out) // an error reply is plain text and leaves this nil
		return resp.StatusCode, out
	}
}

func TestSettingsEndpointIsGuarded(t *testing.T) {
	call := settingsAPI(t, NewManager(t.TempDir(), 2))
	if code, _ := call("GET", "", ""); code != http.StatusUnauthorized {
		t.Errorf("GET without a token = %d, want 401", code)
	}
	if code, _ := call("POST", `{"keep_awake":false}`, "wrong"); code != http.StatusUnauthorized {
		t.Errorf("POST with the wrong token = %d, want 401", code)
	}
	if code, _ := call("DELETE", "", "tok"); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", code)
	}
}

func TestSettingsEndpointReadsAndChangesKeepAwake(t *testing.T) {
	m, _ := managerWithFakePower(t, 2)
	call := settingsAPI(t, m)

	code, got := call("GET", "", "tok")
	if code != 200 || got["keep_awake"] != true || got["can_keep_awake"] != true {
		t.Fatalf("GET = %d %v, want keep_awake and can_keep_awake on by default", code, got)
	}

	code, got = call("POST", `{"keep_awake":false}`, "tok")
	if code != 200 || got["keep_awake"] != false {
		t.Fatalf("POST = %d %v, want the reply to show the new value", code, got)
	}
	if m.KeepAwake() {
		t.Error("the manager did not take the change")
	}

	// Leaving a field out leaves the setting alone.
	if _, got = call("POST", `{}`, "tok"); got["keep_awake"] != false {
		t.Errorf("an empty update changed keep_awake to %v", got["keep_awake"])
	}
	if code, _ = call("POST", `{nonsense`, "tok"); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
}

func TestSettingsEndpointSaysWhatThisComputerCannotDo(t *testing.T) {
	// A manager with nothing connected to the power state, as on a system where
	// godm can neither keep the machine awake nor put it to sleep.
	call := settingsAPI(t, NewManager(t.TempDir(), 2))
	_, got := call("GET", "", "tok")
	if got["can_keep_awake"] != false {
		t.Errorf("can_keep_awake = %v with nothing connected, want false", got["can_keep_awake"])
	}
	if !reflect.DeepEqual(got["after_all_options"], []any{"nothing"}) {
		t.Errorf("after_all_options = %v, want only nothing", got["after_all_options"])
	}
	if code, _ := call("POST", `{"after_all":"shutdown"}`, "tok"); code != http.StatusBadRequest {
		t.Errorf("arming a shutdown that cannot happen = %d, want 400", code)
	}
}

func TestSettingsEndpointArmsAndReportsTheAction(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	call := settingsAPI(t, m)

	_, got := call("GET", "", "tok")
	if got["after_all"] != "nothing" || !reflect.DeepEqual(got["after_all_options"], []any{"nothing", "sleep", "shutdown"}) {
		t.Fatalf("GET = %v, want nothing armed and all three offered", got)
	}
	code, got := call("POST", `{"after_all":"shutdown"}`, "tok")
	if code != 200 || got["after_all"] != "shutdown" {
		t.Fatalf("POST = %d %v, want shutdown armed", code, got)
	}
	if code, _ = call("POST", `{"after_all":"explode"}`, "tok"); code != http.StatusBadRequest {
		t.Errorf("an unknown action = %d, want 400", code)
	}
	if m.AfterAll() != "shutdown" {
		t.Error("a refused request disarmed the action")
	}
	if got := pc.did(); len(got) != 0 {
		t.Errorf("arming the action ran it: %v", got)
	}
}

func TestSettingsEndpointControlsSorting(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	call := settingsAPI(t, m)

	_, got := call("GET", "", "tok")
	folders, _ := got["folders"].(map[string]any)
	if got["sort_by_type"] != false || got["out_dir"] != m.outDir || len(folders) != 6 ||
		folders["video"] != "Video" || folders["audio"] != "Music" || folders["app"] != "Programs" {
		t.Fatalf("GET = %v, want sorting off, the default folder and six default names", got)
	}

	code, got := call("POST", `{"sort_by_type":true,"folders":{"video":"Movies","image":""}}`, "tok")
	folders, _ = got["folders"].(map[string]any)
	if code != 200 || got["sort_by_type"] != true || folders["video"] != "Movies" || folders["image"] != "" || folders["audio"] != "Music" {
		t.Fatalf("POST = %d %v, want only the named folders changed", code, got)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.mkv"}); !strings.HasSuffix(got, "Movies") {
		t.Errorf("the manager did not take the change: %s", got)
	}

	for _, bad := range []string{`{"folders":{"video":"..\\.."}}`, `{"folders":{"video":"a/b"}}`, `{"folders":{"photos":"x"}}`} {
		if code, _ = call("POST", bad, "tok"); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
	if _, got = call("GET", "", "tok"); got["folders"].(map[string]any)["video"] != "Movies" {
		t.Error("a refused request changed a folder")
	}
}

// The page learns about an armed action from the task list it already polls.
func TestTaskListSaysWhatIsArmed(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	s := &server{mgr: m, token: "tok"}
	srv := newTestServer(t, s.guard(s.handleTasks))
	read := func() map[string]any {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if got := read()["after_all"]; got != "nothing" {
		t.Errorf("after_all = %v, want nothing", got)
	}
	arm(t, m, "sleep")
	if got := read()["after_all"]; got != "sleep" {
		t.Errorf("after_all = %v, want sleep", got)
	}
}

// The extension's confirm dialog leaves the folder out unless the person picks
// one, so it needs to know whether godm will sort the file to say where it goes.
func TestConfigTellsTheExtensionAboutSorting(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	s := &server{mgr: m, token: "tok"}
	read := func() map[string]any {
		rec := httptest.NewRecorder()
		s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read()["sort_by_type"]; got != false {
		t.Fatalf("sort_by_type = %v before turning it on, want false", got)
	}
	on := true
	if err := m.UpdateSettings(settingsUpdate{SortByType: &on}); err != nil {
		t.Fatal(err)
	}
	if got := read()["sort_by_type"]; got != true {
		t.Fatalf("sort_by_type = %v after turning it on, want true", got)
	}
}
