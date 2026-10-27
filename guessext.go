package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// A file can come with a name that has no extension: a file host that names
// a film by its hash, say, and serves it as application/octet-stream. Saved
// under that name it is a file Windows cannot open, and the manager does not
// know it is a video, so it offers no Play now. The first bytes of the file
// say what it is far more reliably than anything the server sends, so for
// such a name they are read before the file is named, and the extension they
// call for is added.

// sniffSize is how much of the start of a file is read: enough to reach the
// descriptor a disc image keeps 32 KiB in, and the one after it.
const sniffSize = 36 << 10

// sniffFor is the longest the read may take. A name is not worth holding up a
// download for, so one that takes longer is left as it is.
var sniffFor = 20 * time.Second

// extensionless is true for a name with nothing after its last dot that could
// be an extension: no dot at all, or one followed by words ("Mr. Robot S01E01").
func extensionless(name string) bool {
	ext := filepath.Ext(name)
	return ext == "" || strings.Contains(ext, " ")
}

// sniffHead reads the first sniffSize bytes of the file, nil when they cannot
// be had, which leaves the name as it was.
func sniffHead(ctx context.Context, c *http.Client, o Options) []byte {
	ctx, cancel := context.WithTimeout(ctx, sniffFor)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil
	}
	applyHeaders(req, o.Headers, o.URL)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", sniffSize-1))
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	// Only a ranged answer is the start of the file and no more; the probe has
	// already said the server gives one.
	if resp.StatusCode != http.StatusPartialContent {
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, sniffSize))
	if err != nil {
		return nil
	}
	return head
}

// sniffedExtension is the extension for a file that begins with head, "" for
// one it does not know. It knows films, disc images, a little audio, archives
// and PDF. A program is deliberately not among them: a file the server did not
// call one is never given a name a double click would run.
func sniffedExtension(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		// An EBML header, which Matroska and WebM share; its DocType, a few
		// bytes in, says which of the two it is.
		if bytes.Contains(head[:min(len(head), 64)], []byte("webm")) {
			return ".webm"
		}
		return ".mkv"
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		switch string(head[8:12]) {
		case "qt  ":
			return ".mov"
		case "M4A ":
			return ".m4a"
		case "heic", "heix", "mif1", "msf1", "avif":
			return "" // pictures, in the same box format
		}
		return ".mp4"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return ".avi"
	case transportStream(head, 188, 0):
		return ".ts"
	case transportStream(head, 192, 4):
		// A Blu-ray stream: each packet has a four-byte time stamp before it.
		return ".m2ts"
	case discImage(head):
		return ".iso"
	case bytes.HasPrefix(head, []byte("fLaC")):
		return ".flac"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return ".zip"
	case bytes.HasPrefix(head, []byte("Rar!\x1a\x07")):
		return ".rar"
	case bytes.HasPrefix(head, []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}):
		return ".7z"
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return ".pdf"
	}
	return ""
}

// transportStream is true when head is MPEG transport stream packets of size
// bytes, each with its sync byte at offset into it. Ten in a row is no
// accident.
func transportStream(head []byte, size, offset int) bool {
	const packets = 10
	if len(head) < size*packets {
		return false
	}
	for i := range packets {
		if head[i*size+offset] != 0x47 {
			return false
		}
	}
	return true
}

// discImage is true for an ISO 9660 or UDF image, which name themselves in the
// descriptor 32 KiB in. A Blu-ray image is UDF.
func discImage(head []byte) bool {
	const at = 32<<10 + 1
	if len(head) < at+5 {
		return false
	}
	switch string(head[at : at+5]) {
	case "CD001", "BEA01":
		return true
	}
	return false
}
