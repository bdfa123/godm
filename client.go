package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
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

// daemonRefusal is the daemon answering and saying no to this request, as
// against not being reachable or not understanding the question. The native
// host passes the difference on, because the extension treats an unreachable
// host as a reason to leave downloads to the browser and a refusal is not one.
type daemonRefusal struct{ msg string }

func (e *daemonRefusal) Error() string { return e.msg }

// refusedByDaemon reports whether the daemon was reached and declined the
// request. A 401, 403, 404 or 405 is the host and the daemon not agreeing about
// the token or the API, which is a fault in the setup, not a verdict on a job.
func refusedByDaemon(err error) bool {
	var r *daemonRefusal
	return errors.As(err, &r)
}

// maxReason bounds the reason that is passed on: it ends up in a notification,
// and in a reply to the browser that Chrome drops past 1 MB.
const maxReason = 1000

// refusalReason is the sentence the daemon wrote to say why it refused: the body
// of its error answer, trimmed, and cut short (on a character boundary) if it
// runs on. It is empty when the daemon said nothing.
func refusalReason(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, maxReason+utf8.UTFMax))
	why := strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
	if len(why) > maxReason {
		why = strings.TrimSpace(clip(why, maxReason)) + "..."
	}
	return why
}

// clip cuts s to at most n bytes without cutting a character in two.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func refusalStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return false
	}
	return code >= 400 && code < 500
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
		if refusalStatus(resp.StatusCode) {
			if why := refusalReason(resp.Body); why != "" {
				return &daemonRefusal{why}
			}
			return &daemonRefusal{"daemon returned " + resp.Status}
		}
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

// pid asks the daemon which process it is, which the same ping that shows it
// is alive also says.
func (c *daemonClient) pid() (int, error) {
	var out struct {
		PID int `json:"pid"`
	}
	if err := c.do(http.MethodGet, "/api/ping", nil, &out); err != nil {
		return 0, err
	}
	return out.PID, nil
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

func (c *daemonClient) config() (map[string]any, error) {
	var out map[string]any
	if err := c.do(http.MethodGet, "/api/config", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// browse opens the daemon's folder chooser and waits for the answer. A person
// is choosing, so the usual short timeout would cut them off; the daemon gives
// up on its own after three minutes.
func (c *daemonClient) browse(current string) (string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Path  string `json:"path"`
	}
	slow := *c
	slow.http = &http.Client{Timeout: 4 * time.Minute}
	if err := slow.do(http.MethodPost, "/api/browse?current="+url.QueryEscape(current), nil, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", &daemonRefusal{out.Error}
	}
	return out.Path, nil
}

// inspectTimeout is how long an inspection may take. The daemon gives yt-dlp
// and the playlist fetch 30 seconds and then answers with what went wrong, so
// this waits a little longer than that rather than cutting in just before an
// answer that is on its way.
const inspectTimeout = 35 * time.Second

func (c *daemonClient) inspect(req jobRequest) (*StreamInfo, error) {
	var out struct {
		OK    bool        `json:"ok"`
		Error string      `json:"error"`
		Info  *StreamInfo `json:"info"`
	}
	slow := *c
	slow.http = &http.Client{Timeout: inspectTimeout}
	if err := slow.do(http.MethodPost, "/api/inspect", req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, &daemonRefusal{out.Error}
	}
	return out.Info, nil
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
