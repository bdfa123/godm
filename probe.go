package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
//
// A server that follows the RFC to the letter answers 416 to a range into an
// object with nothing in it (S3 and nginx do), which would make an empty file
// impossible to download. A 416 to this request says nothing about whether the
// URL leads to a file, so the question is asked again without a range: the
// answer is then a plain 200, taken as a stream that cannot be resumed, or the
// real reason the URL does not work.
func Probe(ctx context.Context, c *http.Client, o Options) (*ProbeResult, error) {
	pr, err := probe(ctx, c, o, true)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusRequestedRangeNotSatisfiable {
		return probe(ctx, c, o, false)
	}
	return pr, err
}

func probe(ctx context.Context, c *http.Client, o Options, ranged bool) (*ProbeResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, o.Headers, o.URL)
	if ranged {
		req.Header.Set("Range", "bytes=0-0")
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	defer func() {
		// Draining lets the connection be reused, but a server that ignored
		// the Range and then stopped sending must not hold the download here.
		body := newStallGuard(resp.Body, o.StallTimeout, cancel)
		io.Copy(io.Discard, io.LimitReader(body, 4<<10))
		body.stop()
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

// applyHeaders puts a download's headers on a request. origin is the address
// they came with, the link that was clicked: the cookies and tokens among
// them were the browser's for that site, and a request to anywhere else goes
// without them.
func applyHeaders(req *http.Request, h map[string]string, origin string) {
	login := loginMayGo(origin, req.URL)
	for k, v := range h {
		if v == "" {
			continue
		}
		if !login && loginHeader(k) {
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

// loginHeader reports whether a header says who the user is to a site. They
// are the ones net/http takes off a request it redirects to another site.
func loginHeader(name string) bool {
	switch strings.ToLower(name) {
	case "cookie", "cookie2", "authorization", "www-authenticate",
		"proxy-authorization", "proxy-authenticate":
		return true
	}
	return false
}

// withoutLogin is a copy of headers without the ones loginHeader names.
func withoutLogin(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if !loginHeader(k) {
			out[k] = v
		}
	}
	return out
}

// loginMayGo reports whether a request to dest may carry the cookies and
// tokens that came with the link at origin. The extension hands over the
// browser's cookies for the page's own address, httpOnly ones included, and
// the browser would send them nowhere else. A download seldom stays there: the
// site redirects to a file host, a playlist names segments and keys on a CDN,
// and every one of those requests is made here, straight to that address, with
// no redirect for net/http to police.
//
// So this is the rule net/http applies when it follows a redirect: the same
// host or one below it, since nothing says how wide a cookie handed over as a
// plain header was meant to reach. One thing is added, that what was taken for
// an https page never goes out over plain http, where anyone on the path
// could read it.
func loginMayGo(origin string, dest *url.URL) bool {
	from, err := url.Parse(origin)
	return err == nil && loginInScope(from, dest)
}

func loginInScope(from, dest *url.URL) bool {
	if from.Scheme == "https" && dest.Scheme != "https" {
		return false
	}
	return hostWithin(dest.Hostname(), from.Hostname())
}

// hostWithin reports whether host is parent or a name below it. Ports are
// left out, as they are for cookies.
func hostWithin(host, parent string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	parent = strings.TrimSuffix(strings.ToLower(parent), ".")
	if host == "" || parent == "" {
		return false
	}
	if host == parent {
		return true
	}
	// An address has nothing below it, and the end of an IPv6 zone is not a
	// domain: "::1%.site.example" is not part of site.example.
	if strings.ContainsAny(host, ":%") || net.ParseIP(parent) != nil {
		return false
	}
	return strings.HasSuffix(host, "."+parent)
}

// dropLoginOnRedirect is the client's CheckRedirect. net/http already leaves
// the login off a redirect to another site; this holds a redirect to the rule
// above, which also covers one that drops from https to http on the same
// host. It keeps net/http's limit of ten redirects, which setting a
// CheckRedirect would otherwise remove.
func dropLoginOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !loginInScope(via[0].URL, req.URL) {
		for k := range req.Header {
			if loginHeader(k) {
				delete(req.Header, k)
			}
		}
	}
	return nil
}
