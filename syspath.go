package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// A process keeps the PATH it was started with, and godm's daemon lives long:
// it is usually started by Chrome's native host and stays until the computer
// is shut down. A program installed in the meantime (yt-dlp, with the Deno and
// ffmpeg it needs, or a media player) is on the system's PATH from the moment
// its installer finishes, and on no one's else, godm's own among them, until
// whatever started it is restarted. So when a tool is not found on godm's PATH
// the system's is read again, as many times as it is asked, and a tool that is
// started is given what it needs from there too.

// currentSystemPath lists the folders the system has on PATH now: on Windows
// what the registry holds, elsewhere nothing, since a login shell is what puts
// a PATH together there. It is a variable so a test can stand in for the
// registry.
var currentSystemPath = registryPathDirs

// lookPath is exec.LookPath, which goes by godm's own PATH, followed by the
// system's PATH as it is now.
func lookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return lookPathNow(name)
}

// lookPathNow looks for a program only in the folders currentSystemPath lists.
func lookPathNow(name string) (string, error) {
	for _, dir := range currentSystemPath() {
		for _, cand := range executableNames(name) {
			p := filepath.Join(dir, cand)
			fi, err := os.Stat(p)
			if err != nil || fi.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
				continue
			}
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// executableNames are the file names a command can be found under: as given,
// and on Windows, when it has no extension, with each of the extensions that
// make a file a command.
func executableNames(name string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(name) != "" {
		return []string{name}
	}
	exts := os.Getenv("PATHEXT")
	if exts == "" {
		exts = ".COM;.EXE;.BAT;.CMD"
	}
	var out []string
	for _, ext := range strings.Split(exts, ";") {
		if ext != "" {
			out = append(out, name+strings.ToLower(ext))
		}
	}
	return out
}

// cleanPathDirs tidies a list of folders read from a PATH: no blanks, no
// quotes, nothing that is not spelled out in full (a PATH entry that is not
// would be searched from wherever the program happens to be, which is not a
// place to look for what to run) and no folder twice.
func cleanPathDirs(dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.Trim(strings.TrimSpace(d), `"`)
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		if k := pathKey(d); !seen[k] {
			seen[k] = true
			out = append(out, d)
		}
	}
	return out
}

// pathKey is what two spellings of one folder have in common.
func pathKey(dir string) string {
	dir = filepath.Clean(strings.Trim(strings.TrimSpace(dir), `"`))
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	return dir
}

// expandPercent replaces each %NAME% with the value get has for it, as the
// system does for the entries of a PATH kept as an expandable string. A name
// nobody knows is left as written, which is also what the system does.
func expandPercent(s string, get func(string) string) string {
	var out strings.Builder
	for {
		start := strings.IndexByte(s, '%')
		if start < 0 {
			break
		}
		end := strings.IndexByte(s[start+1:], '%')
		if end < 0 {
			break
		}
		end += start + 1
		name := s[start+1 : end]
		val := ""
		if name != "" {
			val = get(name)
		}
		if val == "" {
			// Not a variable we know: it stays as written, closing sign and all,
			// as the system leaves it.
			out.WriteString(s[:end+1])
			s = s[end+1:]
			continue
		}
		out.WriteString(s[:start])
		out.WriteString(val)
		s = s[end+1:]
	}
	out.WriteString(s)
	return out.String()
}

// sameEnvName compares two names of environment variables the way the system
// does.
func sameEnvName(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// setEnv returns env with name set to value, replacing any spelling of it
// already there.
func setEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && sameEnvName(k, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, name+"="+value)
}

// withPathDirs returns env with the folders in extra added to the end of its
// PATH, unless they are on it already. The folders it has come first: they are
// the ones the user's own setup for this process asked for.
func withPathDirs(env []string, extra []string) []string {
	at, key, cur := -1, "PATH", ""
	for i, kv := range env {
		// The last one wins, as it does when a program is started.
		if k, v, ok := strings.Cut(kv, "="); ok && sameEnvName(k, "PATH") {
			at, key, cur = i, k, v
		}
	}
	have := map[string]bool{}
	for _, d := range filepath.SplitList(cur) {
		have[pathKey(d)] = true
	}
	list, changed := cur, false
	for _, d := range extra {
		if k := pathKey(d); !have[k] {
			have[k] = true
			if list != "" && !strings.HasSuffix(list, string(os.PathListSeparator)) {
				list += string(os.PathListSeparator)
			}
			list += d
			changed = true
		}
	}
	if !changed {
		return env
	}
	out := slices.Clone(env)
	if at >= 0 {
		out[at] = key + "=" + list
	} else {
		out = append(out, key+"="+list)
	}
	return out
}
