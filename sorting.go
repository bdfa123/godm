package main

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// folderKind is one sort of file that can be given a folder of its own. The key
// is the name the page's kindOf uses for the same extensions, so keep the lists
// in step with it; a test compares them.
type folderKind struct {
	key    string
	folder string // the name used until the user picks another
	exts   []string
}

var folderKinds = []folderKind{
	{"video", "Video", []string{"mp4", "mkv", "avi", "mov", "webm", "flv", "wmv", "m4v", "ts"}},
	{"audio", "Music", []string{"mp3", "flac", "wav", "aac", "m4a", "ogg", "opus"}},
	{"archive", "Archives", []string{"zip", "rar", "7z", "gz", "xz", "bz2", "tar", "zst", "tgz", "iso", "img", "dmg"}},
	{"doc", "Documents", []string{"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "epub", "txt", "csv"}},
	{"app", "Programs", []string{"exe", "msi", "apk", "deb", "rpm", "pkg", "appimage", "jar", "whl"}},
	{"image", "Images", []string{"jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "heic"}},
}

var kindByExt = func() map[string]string {
	m := map[string]string{}
	for _, k := range folderKinds {
		for _, e := range k.exts {
			m[e] = k.key
		}
	}
	return m
}()

func defaultFolders() map[string]string {
	m := make(map[string]string, len(folderKinds))
	for _, k := range folderKinds {
		m[k.key] = k.folder
	}
	return m
}

func lowerExt(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

// folderKindFor says what sort of file a request will produce, from what is
// known before anything is fetched, or "" when it cannot tell. Streams and
// yt-dlp jobs are video whatever their address looks like. For anything else the
// name the file will be given decides if it has an extension; when it has none
// the address does, with its query left out, since that is where a pasted link
// carries the name. A name that arrives only in the server's headers cannot be
// known yet.
func folderKindFor(req jobRequest) string {
	if isYTDLPJob(req) || isHLSJob(req) {
		return "video"
	}
	if ext := lowerExt(baseName(req.Filename)); ext != "" {
		return kindByExt[ext]
	}
	if u, err := url.Parse(req.URL); err == nil {
		return kindByExt[lowerExt(basenameFromURL(u))]
	}
	return ""
}

// downloadDir is where a new download goes. A folder the caller names always
// wins. Otherwise it is the default folder or, with sorting on, the subfolder
// for what is being downloaded. It is decided once, when the task is added, and
// kept with the task: turning sorting on or off, or renaming a folder, never
// moves anything that already exists.
func (m *Manager) downloadDir(req jobRequest) string {
	if req.OutDir != "" {
		return req.OutDir
	}
	m.mu.Lock()
	on, folders := m.settings.SortByType, m.settings.Folders
	var name string
	if on {
		name = folders[folderKindFor(req)] // a type that is not known has no folder
	}
	m.mu.Unlock()
	if name == "" {
		return m.outDir
	}
	return filepath.Join(m.outDir, name)
}

// cleanFolderName checks a folder name typed into the dialog. It must be one
// plain folder name, not a path: "..\..\Windows" is what it must never become.
// Empty is allowed and keeps that sort of file in the main folder.
func cleanFolderName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", nil
	case !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100:
		return "", fmt.Errorf("a folder name has to be plain text of up to 100 characters")
	// winIllegal does not list the backslash (inside its brackets "\|" is a
	// pipe, since the file-name code strips directories before using it), and
	// here a separator is exactly the thing to refuse.
	case strings.ContainsAny(name, `\/`) || winIllegal.MatchString(name):
		return "", fmt.Errorf("%q cannot be a folder name: it may not contain \\ / : * ? \" < > |", name)
	case strings.HasSuffix(name, "."):
		return "", fmt.Errorf("%q cannot be a folder name: Windows does not allow a name that ends in a dot", name)
	case winReserved.MatchString(name):
		return "", fmt.Errorf("%q is a name Windows keeps for itself", name)
	}
	return name, nil
}

// folderNames tidies a set of names read from disk, so that a hand-edited
// tasks.json cannot send a download anywhere but under the default folder.
// Anything missing or unusable gets its default.
func folderNames(have map[string]string) map[string]string {
	out := defaultFolders()
	for key := range out {
		if name, ok := have[key]; ok {
			if clean, err := cleanFolderName(name); err == nil {
				out[key] = clean
			}
		}
	}
	return out
}
