package main

import (
	"encoding/json"
	"io"
	"net/http"
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
	// godm cannot keep the machine awake.
	call := settingsAPI(t, NewManager(t.TempDir(), 2))
	if _, got := call("GET", "", "tok"); got["can_keep_awake"] != false {
		t.Errorf("can_keep_awake = %v with nothing connected, want false", got["can_keep_awake"])
	}
}
