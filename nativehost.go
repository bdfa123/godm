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
	Type        string            `json:"type"` // download | tasks | ping
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	Referrer    string            `json:"referrer"`
	Cookie      string            `json:"cookie"`
	UserAgent   string            `json:"userAgent"`
	Headers     map[string]string `json:"headers"`
	Connections int               `json:"connections"`
	OutDir      string            `json:"outDir"`
}

type nativeResponse struct {
	OK    bool       `json:"ok"`
	ID    string     `json:"id,omitempty"`
	Error string     `json:"error,omitempty"`
	Tasks []TaskView `json:"tasks,omitempty"`
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
		// Carry the browser identity across. Miss any of these and an
		// authenticated download turns into a 403 or an HTML login page.
		headers := map[string]string{}
		for k, v := range req.Headers {
			headers[k] = v
		}
		if req.Cookie != "" {
			headers["Cookie"] = req.Cookie
		}
		if req.Referrer != "" {
			headers["Referer"] = req.Referrer
		}
		if req.UserAgent != "" {
			headers["User-Agent"] = req.UserAgent
		}
		id, err := c.submit(jobRequest{
			URL:         req.URL,
			Filename:    req.Filename,
			Headers:     headers,
			Connections: req.Connections,
			OutDir:      req.OutDir,
		})
		if err != nil {
			log.Printf("submit failed: %v", err)
			return nativeResponse{Error: err.Error()}
		}
		log.Printf("accepted %s -> %s", req.URL, id)
		return nativeResponse{OK: true, ID: id}

	default:
		return nativeResponse{Error: "unknown message type: " + req.Type}
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
