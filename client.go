package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// daemonClient talks to a running daemon over loopback.
type daemonClient struct {
	base  string
	token string
	http  *http.Client
}

func newDaemonClient() (*daemonClient, error) {
	port := readPort()
	if port == 0 {
		return nil, fmt.Errorf("daemon not running (no port file)")
	}
	tok := readToken()
	if tok == "" {
		return nil, fmt.Errorf("no token file; run: godm daemon")
	}
	return &daemonClient{
		base:  fmt.Sprintf("http://127.0.0.1:%d", port),
		token: tok,
		http:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (c *daemonClient) do(method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *daemonClient) ping() error {
	return c.do(http.MethodGet, "/api/ping", nil, &struct{}{})
}

func (c *daemonClient) submit(req jobRequest) (string, error) {
	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/download", req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *daemonClient) submitBatch(reqs []jobRequest) ([]string, []string, error) {
	var out struct {
		IDs    []string `json:"ids"`
		Errors []string `json:"errors"`
	}
	body := map[string]any{"items": reqs}
	if err := c.do(http.MethodPost, "/api/batch", body, &out); err != nil {
		return nil, nil, err
	}
	return out.IDs, out.Errors, nil
}

func (c *daemonClient) tasks() ([]TaskView, error) {
	var out struct {
		Tasks []TaskView `json:"tasks"`
	}
	if err := c.do(http.MethodGet, "/api/tasks", nil, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// ensureDaemon returns a client, starting a detached daemon first if needed.
func ensureDaemon() (*daemonClient, error) {
	if c, err := newDaemonClient(); err == nil && c.ping() == nil {
		return c, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := spawnDetached(exe, "daemon"); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if c, err := newDaemonClient(); err == nil && c.ping() == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("daemon did not come up within 8s (see %s)", logPath())
}
