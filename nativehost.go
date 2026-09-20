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

type nativeRequest struct {
	Type        string            `json:"type"` // download | batch | inspect | tasks | ping
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
	// UI carries the authenticated manager URL so the popup can open it
	// without the extension ever storing the daemon token.
	UI string `json:"ui,omitempty"`
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

	uiURL := c.base + "/?token=" + c.token

	switch req.Type {
	case "ping":
		return nativeResponse{OK: true, UI: uiURL}

	case "tasks":
		tasks, err := c.tasks()
		if err != nil {
			return nativeResponse{Error: err.Error()}
		}
		return nativeResponse{OK: true, Tasks: tasks, UI: uiURL}

	case "download", "":
		id, err := c.submit(req.job(req.URL, req.Filename, req.Cookie))
		if err != nil {
			log.Printf("submit failed: %v", err)
			return nativeResponse{Error: err.Error()}
		}
		log.Printf("accepted %s -> %s", req.URL, id)
		return nativeResponse{OK: true, ID: id}

	case "config":
		cfg, err := c.config()
		if err != nil {
			return nativeResponse{Error: err.Error()}
		}
		return nativeResponse{OK: true, Config: cfg, UI: uiURL}

	case "inspect":
		info, err := c.inspect(req.job(req.URL, req.Filename, req.Cookie))
		if err != nil {
			return nativeResponse{Error: err.Error()}
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
			return nativeResponse{Error: err.Error()}
		}
		log.Printf("accepted batch of %d from %s", len(ids), req.Referrer)
		return nativeResponse{OK: len(ids) > 0, IDs: ids, Errors: errs}

	default:
		return nativeResponse{Error: "unknown message type: " + req.Type}
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
	var hdr [4]byte
	binary.NativeEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
