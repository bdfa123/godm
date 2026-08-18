package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ProbeResult is what one cheap request tells us about a URL before we commit
// to a download plan.
type ProbeResult struct {
	FinalURL  string // after the full redirect chain
	Size      int64  // -1 when the server won't say
	Resumable bool   // true only if we saw a real 206
	Filename  string
	MIME      string
	ETag      string
}

// Probe issues a one-byte ranged GET. HEAD is unreliable in the wild: plenty of
// CDNs 405 it, and some report Accept-Ranges on HEAD then ignore Range on GET.
// Asking for bytes 0-0 costs nothing and gives us the truth.
func Probe(ctx context.Context, c *http.Client, o Options) (*ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, o.Headers)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	pr := &ProbeResult{
		FinalURL: resp.Request.URL.String(),
		Size:     -1,
		MIME:     resp.Header.Get("Content-Type"),
		ETag:     resp.Header.Get("ETag"),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
			pr.Size = total
			pr.Resumable = true
		}
	case http.StatusOK:
		// Server ignored Range entirely -> single connection, no resume.
		if resp.ContentLength >= 0 {
			pr.Size = resp.ContentLength
		}
	default:
		if resp.StatusCode >= 400 {
			// Return a typed error so retry policy is decided on the status:
			// 429/503 are worth waiting out, 403/404 are not.
			return nil, &statusError{
				code:       resp.StatusCode,
				status:     resp.Status,
				retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			}
		}
	}

	pr.Filename = ResolveFilename(o.Filename, resp)
	return pr, nil
}

// parseContentRangeTotal reads the total size out of "bytes 0-0/1234".
// A "*" total means the server is streaming and we can't plan segments.
func parseContentRangeTotal(v string) (int64, bool) {
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0, false
	}
	total := strings.TrimSpace(v[i+1:])
	if total == "" || total == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func applyHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		if v == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "range", "host", "content-length":
			continue // ours to control
		case "referer", "referrer":
			req.Header.Set("Referer", v)
		default:
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUA)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
}
