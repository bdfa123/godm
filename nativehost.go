package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
)

// Chrome caps a single browser-to-host message at 64 MB and a host-to-browser
// message at 1 MB. We stay far below both.
const maxNativeMessage = 8 << 20

// maxHostReply is the most the host will write to the browser in one message,
// under Chrome's 1 MB so that a reply too big is replaced by an explanation and
// not by the browser closing the connection.
const maxHostReply = 900 << 10

// popupTaskLimit is how many downloads the popup lists. It is sent no more.
const popupTaskLimit = 8

type nativeRequest struct {
	Type        string            `json:"type"` // download | batch | inspect | config | browse | tasks | open | ping
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	Referrer    string            `json:"referrer"`
	Cookie      string            `json:"cookie"`
	UserAgent   string            `json:"userAgent"`
	Headers     map[string]string `json:"headers"`
	Connections int               `json:"connections"`
	OutDir      string            `json:"outDir"`
	Items       []batchItem       `json:"items"`
	// Kind says outright that the URL is a stream playlist. Variant picks a
	// quality from it; absent means take the best on offer.
	Kind    string `json:"kind"`
	Variant *int   `json:"variant"`
	// Current is the folder a "browse" request should open the chooser at.
	Current string `json:"current"`
}

// batchItem is one link picked from a page. Cookies are per item because the
// links on one page can point at several hosts.
type batchItem struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Cookie   string `json:"cookie"`
}

type nativeResponse struct {
	OK     bool        `json:"ok"`
	ID     string      `json:"id,omitempty"`
	IDs    []string    `json:"ids,omitempty"`
	Errors []string    `json:"errors,omitempty"`
	Error  string      `json:"error,omitempty"`
	Tasks  []TaskView  `json:"tasks,omitempty"`
	Info   *StreamInfo `json:"info,omitempty"`
	// Config lets the popup ask what the daemon can do — whether yt-dlp is
	// installed, where downloads land — without holding the daemon token.
	Config map[string]any `json:"config,omitempty"`
	// Path is the folder picked in a "browse" request. It is empty with OK set
	// when the user closed the chooser without picking anything.
	Path string `json:"path,omitempty"`
	// Rejected says the daemon was reached and turned the request down. The
	// extension reads an answer without it as a host that cannot do its job, and
	// stops handing downloads over for a while; a refusal says nothing against
	// the host, only against the one link.
	Rejected bool `json:"rejected,omitempty"`
}

// RunNativeHost speaks the Chrome native-messaging protocol on stdio:
// a 4-byte native-endian length prefix followed by a UTF-8 JSON payload.
// It is a thin client: the daemon owns the actual transfers, so downloads
// survive the MV3 service worker being torn down.
func RunNativeHost() error {
	lf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		log.SetOutput(lf)
		defer lf.Close()
	}
	log.SetPrefix("[native-host] ")
	log.Printf("started, pid=%d", os.Getpid())

	in := os.Stdin
	out := os.Stdout

	for {
		msg, err := readNativeMessage(in)
		if err == io.EOF {
			log.Printf("port closed, exiting")
			return nil
		}
		if err != nil {
			log.Printf("read error: %v", err)
			return err
		}

		var req nativeRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			writeNativeMessage(out, nativeResponse{Error: "bad json: " + err.Error()})
			continue
		}
		resp := handleNative(req)
		if err := writeNativeMessage(out, resp); err != nil {
			log.Printf("write error: %v", err)
			return err
		}
	}
}

func handleNative(req nativeRequest) nativeResponse {
	c, err := ensureDaemon()
	if err != nil {
		log.Printf("daemon unavailable: %v", err)
		return nativeResponse{Error: err.Error()}
	}
	return forwardNative(c, req)
}

// forwardNative is the part of handleNative that needs a daemon to talk to,
// split out so it can be tested against a stand-in for one.
//
// Nothing it answers carries the daemon token. Whatever the extension is told
// it can pass on, and an address it opens in a tab is there for every other
// extension to read, so the token stays on this side of the pipe.
func forwardNative(c *daemonClient, req nativeRequest) nativeResponse {
	switch req.Type {
	case "ping":
		return nativeResponse{OK: true}

	case "tasks":
		tasks, err := c.tasks()
		if err != nil {
			return failed(err)
		}
		return nativeResponse{OK: true, Tasks: popupTasks(tasks)}

	case "open":
		// The popup's link to the manager. The address of the page has the
		// token in it, so it is opened from here and not handed back to be
		// opened as a tab.
		if err := openManager(); err != nil {
			log.Printf("could not open the manager: %v", err)
			return nativeResponse{Error: err.Error()}
		}
		return nativeResponse{OK: true}

	case "download", "":
		id, err := c.submit(req.job(req.URL, req.Filename, req.Cookie))
		if err != nil {
			log.Printf("submit failed: %v", err)
			return failed(err)
		}
		log.Printf("accepted %s -> %s", req.URL, id)
		return nativeResponse{OK: true, ID: id}

	case "config":
		cfg, err := c.config()
		if err != nil {
			return failed(err)
		}
		return nativeResponse{OK: true, Config: cfg}

	case "browse":
		// The native folder chooser belongs to the daemon's tray thread, so the
		// confirmation dialog asks for it through here.
		letDaemonTakeForeground(c)
		path, err := c.browse(req.Current)
		if err != nil {
			return failed(err)
		}
		return nativeResponse{OK: true, Path: path}

	case "inspect":
		info, err := c.inspect(req.job(req.URL, req.Filename, req.Cookie))
		if err != nil {
			return failed(err)
		}
		return nativeResponse{OK: true, Info: info}

	case "batch":
		jobs := make([]jobRequest, 0, len(req.Items))
		for _, it := range req.Items {
			jobs = append(jobs, req.job(it.URL, it.Filename, it.Cookie))
		}
		ids, errs, err := c.submitBatch(jobs)
		if err != nil {
			log.Printf("batch submit failed: %v", err)
			return failed(err)
		}
		log.Printf("accepted batch of %d from %s", len(ids), req.Referrer)
		// A batch with nothing in it the daemon would take has been answered, and
		// the reasons are in Errors.
		return nativeResponse{OK: len(ids) > 0, IDs: ids, Errors: errs, Rejected: len(ids) == 0}

	default:
		return nativeResponse{Error: "unknown message type: " + req.Type}
	}
}

// popupTasks cuts the daemon's task list down to what the popup draws: the
// first few, in the daemon's order, with only the fields it reads. Finished
// downloads stay in the list until they are removed, so the whole of it grows
// without limit and would one day be more than the browser takes in a single
// message from the host.
//
// The popup tells a download fetched in several pieces from one fetched in a
// piece by how many segments it has, so up to two empty ones are kept to say
// so. An address or a name is clipped, not dropped: it stands in for the name
// of a download that has none yet.
func popupTasks(tasks []TaskView) []TaskView {
	if len(tasks) > popupTaskLimit {
		tasks = tasks[:popupTaskLimit]
	}
	out := make([]TaskView, len(tasks))
	for i, t := range tasks {
		out[i] = TaskView{
			ID:       t.ID,
			URL:      clip(t.URL, 2000),
			Filename: clip(t.Filename, 500),
			State:    t.State,
			Size:     t.Size,
			Received: t.Received,
			Speed:    t.Speed,
			Active:   t.Active,
			Segments: make([]SegmentView, min(len(t.Segments), 2)),
		}
	}
	return out
}

// failed is the answer for a request the daemon did not carry out.
func failed(err error) nativeResponse {
	return nativeResponse{Error: err.Error(), Rejected: refusedByDaemon(err)}
}

// allowForeground is a variable so a test can see it called without changing
// what is in front on the machine running the tests.
var allowForeground = allowSetForegroundWindow

// openManager is a variable for the same reason: a test must not put a window
// on the screen of the machine it runs on.
var openManager = showManager

// letDaemonTakeForeground lets the daemon bring its folder chooser in front of
// the browser. Windows refuses that to a process that is not in front itself,
// was not started by whatever is, and was not handed the right by it, and the
// daemon is none of these: the chooser would open behind the browser or only
// flash in the taskbar. This host is the browser's child, so it can hand the
// right over. It is done for each chooser, right before asking for it,
// because the right does not last.
//
// A failure is only logged. The chooser still opens, perhaps behind.
func letDaemonTakeForeground(c *daemonClient) {
	pid, err := c.pid()
	if err != nil {
		log.Printf("could not ask the daemon for its process id: %v; the folder chooser may open behind the browser", err)
		return
	}
	if pid <= 0 {
		log.Printf("the daemon did not say which process it is; the folder chooser may open behind the browser")
		return
	}
	if err := allowForeground(pid); err != nil {
		log.Printf("could not let the daemon (pid %d) take the foreground: %v", pid, err)
	}
}

// job carries the browser identity across. Miss any of these and an
// authenticated download turns into a 403 or an HTML login page. The referrer
// is also kept separately: it is the page to reopen when the link expires.
func (req nativeRequest) job(url, filename, cookie string) jobRequest {
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	if cookie != "" {
		headers["Cookie"] = cookie
	}
	if req.Referrer != "" {
		headers["Referer"] = req.Referrer
	}
	if req.UserAgent != "" {
		headers["User-Agent"] = req.UserAgent
	}
	variant := -1
	if req.Variant != nil {
		variant = *req.Variant
	}
	return jobRequest{
		URL:         url,
		Filename:    filename,
		Referrer:    req.Referrer,
		Headers:     headers,
		Connections: req.Connections,
		OutDir:      req.OutDir,
		Kind:        req.Kind,
		Variant:     variant,
	}
}

func readNativeMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}
	n := binary.NativeEndian.Uint32(hdr[:])
	if n == 0 || n > maxNativeMessage {
		return nil, fmt.Errorf("refusing %d byte message", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeNativeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxHostReply {
		// Chrome would drop this and close the host, which the extension can only
		// read as the host being gone. Say what happened instead.
		log.Printf("reply of %d bytes is too big for the browser", len(b))
		b, err = json.Marshal(nativeResponse{Error: fmt.Sprintf("the reply was %d bytes, more than the browser accepts from a native host", len(b))})
		if err != nil {
			return err
		}
	}
	var hdr [4]byte
	binary.NativeEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
