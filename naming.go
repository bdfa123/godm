package main

import (
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	winIllegal   = regexp.MustCompile(`[<>:"/\|?*\x00-\x1f]`)
	winReserved  = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\.|$)`)
	multiSpaceRe = regexp.MustCompile(`\s+`)
)

// ResolveFilename picks a filename in priority order:
// explicit override > Content-Disposition filename* (RFC 5987) > filename= >
// URL path basename > "download". The result is always safe for NTFS.
func ResolveFilename(override string, resp *http.Response) string {
	if n := sanitize(override); n != "" {
		return n
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			// mime.ParseMediaType already decodes RFC 5987 filename* into "filename".
			if n := sanitize(params["filename"]); n != "" {
				return n
			}
		}
		// Fall back to a dumb scan for servers that emit malformed headers.
		if n := sanitize(scrapeFilenameParam(cd)); n != "" {
			return n
		}
	}
	u := resp.Request.URL
	if n := sanitize(basenameFromURL(u)); n != "" {
		return n
	}
	return "download"
}

func scrapeFilenameParam(cd string) string {
	for _, part := range strings.Split(cd, ";") {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)
		if !strings.HasPrefix(lower, "filename=") {
			continue
		}
		v := strings.TrimSpace(part[len("filename="):])
		v = strings.Trim(v, `"'`)
		if dec, err := url.PathUnescape(v); err == nil {
			v = dec
		}
		return v
	}
	return ""
}

func basenameFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" {
		return ""
	}
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	return name
}

// sanitize strips anything NTFS rejects and refuses non-UTF-8 garbage, which is
// how GBK-encoded Content-Disposition headers show up in Go.
func sanitize(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) {
		return ""
	}
	// A server may hand back a full path; keep only the last component.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = winIllegal.ReplaceAllString(name, "_")
	name = multiSpaceRe.ReplaceAllString(name, " ")
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if winReserved.MatchString(name) {
		name = "_" + name
	}
	if len(name) > 180 {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = trimUTF8(name[:len(name)-len(ext)], 180-len(ext)) + ext
	}
	return name
}

func trimUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
