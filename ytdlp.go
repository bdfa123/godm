package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Some sites hand their video out through a protocol of their own rather than
// a playlist anyone can read. Keeping up with those is a full-time job that a
// whole project already does, so godm drives yt-dlp for them instead of
// carrying its own copy of that arms race. Everything the user sees — the
// list, the progress, pausing — stays the same.

// YTDLPMissingError is what the caller shows when the tool is not installed.
// It carries the advice with it so the message is the same everywhere.
type YTDLPMissingError struct{}

func (e *YTDLPMissingError) Error() string {
	return "yt-dlp was not found. Install it (winget install yt-dlp.yt-dlp) or put " +
		"yt-dlp.exe next to godm.exe, then try again"
}

// ytdlpNames are tried in order on PATH.
var ytdlpNames = []string{"yt-dlp", "yt-dlp.exe", "yt-dlp_x86.exe"}

// findYTDLP looks beside godm first so a downloaded copy needs no PATH change,
// then falls back to PATH.
func findYTDLP() (string, error) {
	if custom := strings.TrimSpace(os.Getenv("GODM_YTDLP")); custom != "" {
		if _, err := os.Stat(custom); err == nil {
			return custom, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range ytdlpNames {
			cand := filepath.Join(dir, name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	for _, name := range ytdlpNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", &YTDLPMissingError{}
}

// YTDLPAvailable reports whether the tool can be found, for the UI to offer it
// only when it would work.
func YTDLPAvailable() bool {
	_, err := findYTDLP()
	return err == nil
}

type YTDLPOptions struct {
	URL      string
	OutDir   string
	Headers  map[string]string
	Format   string // yt-dlp format selector; empty means its own default
	Binary   string // overrides discovery
	OnStart  func(YTStart)
	OnUpdate func(YTProgress)
}

type YTStart struct {
	Title string
}

type YTProgress struct {
	Stage      string // "video", "audio" or "media"
	Status     string // yt-dlp's own: downloading, finished, error
	Downloaded int64
	Total      int64
	Speed      int64
	ETA        int64
}

// progressMarker and fileMarker prefix the two things we ask yt-dlp to print,
// so its lines can be picked out of everything else it says.
const (
	progressMarker = "godm-progress|"
	fileMarker     = "godm-file|"
)

const ytProgressTemplate = progressMarker +
	"%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|" +
	"%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|%(info.vcodec)s"

// ytdlpDrain is how long yt-dlp's output is still read after yt-dlp itself has
// gone. Normally it ends that same instant; when something yt-dlp started and
// did not wait for holds it open, this is how long that is allowed to keep
// godm waiting. It is a variable so a test need not sit through it.
var ytdlpDrain = 2 * time.Second

// newYTDLPCmd is a variable so a test can point the runner at a stand-in
// program without yt-dlp being installed.
var newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}

// ytdlpConfigArgs keeps yt-dlp from taking a downloaded file for its
// configuration. Left to itself it reads a yt-dlp.conf from the folder given
// to -P (the current folder when there is no -P) and from the folder its own
// binary is in, and every line of one is an option, --exec among them. The
// first is where godm saves files under names that servers choose; the second
// is that same folder for anyone who keeps yt-dlp next to a godm.exe they run
// from where it was downloaded. So it is told to look nowhere, and is handed
// the one file that is the user's own. A yt-dlp.conf kept beside yt-dlp on
// purpose goes unread with the rest: nothing tells it from one that a download
// left there.
func ytdlpConfigArgs() []string {
	args := []string{"--ignore-config"}
	if conf := ytdlpUserConfig(); conf != "" {
		args = append(args, "--config-locations", conf)
	}
	return args
}

// ytdlpUserConfig finds the user's configuration where yt-dlp would have: the
// "User Configuration" list in its README, in that order, the first file that
// can be opened. It is empty when there is none.
func ytdlpUserConfig() string {
	// yt-dlp lets HOME stand in for the profile folder on Windows.
	home := ""
	if runtime.GOOS == "windows" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	var files []string
	// Each place is a folder of yt-dlp's own, with yt-dlp.conf allowed to sit
	// beside that folder instead of in it.
	place := func(parent, folder string) {
		files = append(files, filepath.Join(parent, "yt-dlp.conf"))
		if strings.HasPrefix(folder, ".") {
			files = append(files, filepath.Join(parent, "yt-dlp.conf.txt"))
		}
		files = append(files,
			filepath.Join(parent, folder, "config"),
			filepath.Join(parent, folder, "config.txt"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		place(xdg, "yt-dlp")
	} else if home != "" {
		place(filepath.Join(home, ".config"), "yt-dlp")
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		place(appdata, "yt-dlp")
	}
	if home != "" {
		place(home, ".yt-dlp")
	}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		f.Close()
		if err != nil || fi.IsDir() {
			continue
		}
		// A file named on the command line that is not there stops yt-dlp, so
		// the name must not depend on which folder yt-dlp is started in.
		if abs, err := filepath.Abs(file); err == nil {
			return abs
		}
		return file
	}
	return ""
}

// runFromOwnFolder starts yt-dlp in the folder it lives in rather than in
// godm's, which is wherever godm happened to be started: the download folder,
// for a godm.exe double-clicked where it was saved. A script standing in for
// yt-dlp, or the Python under it, may load what it finds in the current folder
// whatever yt-dlp was told about configuration, and the folder the program
// came from is the one that has to be trusted anyway.
func runFromOwnFolder(cmd *exec.Cmd) {
	// A relative path would be looked up from the new folder.
	if abs, err := filepath.Abs(cmd.Path); err == nil {
		cmd.Path = abs
	}
	cmd.Dir = filepath.Dir(cmd.Path)
}

func ytdlpArgs(o YTDLPOptions) []string {
	args := []string{
		"--newline",       // one progress update per line instead of carriage returns
		"--no-colors",     // no escape codes to strip back out
		"--no-playlist",   // a URL carrying a list must not fetch two hundred videos
		"--no-overwrites", // never write over a file that is already there
		"--progress",
		"--progress-template", ytProgressTemplate,
		"--no-simulate", // --print would otherwise only pretend
		"--print", "after_move:" + fileMarker + "%(filepath)s",
		"--retries", "10",
		"--fragment-retries", "10",
		"-P", o.OutDir,
	}
	args = append(ytdlpConfigArgs(), args...)
	if o.Format != "" {
		args = append(args, "-f", o.Format)
	}
	for k, v := range o.Headers {
		if v == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "user-agent":
			args = append(args, "--user-agent", v)
		case "referer", "referrer":
			args = append(args, "--referer", v)
		default:
			args = append(args, "--add-header", k+":"+v)
		}
	}
	// Everything after this is a URL, never a flag, whatever it starts with.
	return append(args, "--", o.URL)
}

// RunYTDLP downloads one video and reports where it landed.
func RunYTDLP(ctx context.Context, o YTDLPOptions) (string, error) {
	bin := o.Binary
	if bin == "" {
		found, err := findYTDLP()
		if err != nil {
			return "", err
		}
		bin = found
	}
	if !strings.HasPrefix(strings.ToLower(o.URL), "http://") &&
		!strings.HasPrefix(strings.ToLower(o.URL), "https://") {
		return "", errors.New("only http and https URLs can be handed to yt-dlp")
	}
	// yt-dlp is not started in godm's folder, so a folder named from there has
	// to be spelled out in full or it would be made inside yt-dlp's.
	if abs, err := filepath.Abs(o.OutDir); err == nil {
		o.OutDir = abs
	}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return "", err
	}
	cmd := newYTDLPCmd(ctx, bin, ytdlpArgs(o))
	runFromOwnFolder(cmd)
	// Progress goes to stderr and the printed path to stdout. Both ends of one
	// pipe keeps them in order and means neither can fill its own buffer and
	// wedge the process.
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	// yt-dlp is rarely one process: the released yt-dlp.exe is a launcher with
	// the real program as its child, and ffmpeg is started for merges and for
	// some streams. Pausing has to end all of them, or the rest carries on
	// downloading behind a task that says it stopped.
	release, err := startTree(cmd)
	if err != nil {
		pw.Close()
		pr.Close()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("could not start yt-dlp: %w", err)
	}
	defer release()
	// The child holds its own copy; ours has to go or the read never ends.
	pw.Close()
	defer pr.Close()
	// The read below ends when the last process holding the other end lets
	// go, and that is not always yt-dlp. So its exit is waited for separately,
	// and the pipe is closed under the reader when there is no point reading
	// on: at once when told to stop, since nothing said after that matters,
	// and a little after yt-dlp has gone, since everything it said is in the
	// pipe by then and anything else holding it is not ours to wait for.
	exited, read := make(chan struct{}), make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	go func() {
		select {
		case <-read:
			return
		case <-ctx.Done():
		case <-exited:
			drain := time.NewTimer(ytdlpDrain)
			defer drain.Stop()
			select {
			case <-read:
				return
			case <-ctx.Done():
			case <-drain.C:
			}
		}
		pr.Close()
	}()
	var (
		path  string
		tail  []string
		stage string
	)
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if got, ok := strings.CutPrefix(line, fileMarker); ok {
			path = strings.TrimSpace(got)
			continue
		}
		if up, ok := parseYTProgress(line); ok {
			// Only the line that starts a stream carries its codec; the rest
			// of that stream's updates inherit the label.
			if up.Stage != "" {
				stage = up.Stage
			} else {
				up.Stage = stage
			}
			if o.OnUpdate != nil {
				o.OnUpdate(up)
			}
			continue
		}
		if t := ytdlpTitle(line); t != "" && o.OnStart != nil {
			o.OnStart(YTStart{Title: t})
		}
		// Keep the last few lines: when yt-dlp fails, its reason is in them.
		tail = append(tail, line)
		if len(tail) > 12 {
			tail = tail[len(tail)-12:]
		}
	}
	close(read)
	<-exited
	if ctx.Err() != nil && (waitErr != nil || path == "") {
		// Stopped on request, however far it had got.
		return "", ctx.Err()
	}
	if waitErr != nil {
		return "", fmt.Errorf("yt-dlp failed: %s", ytdlpReason(tail, waitErr))
	}
	if path == "" {
		return "", fmt.Errorf("yt-dlp finished but said nothing about where the file went: %s",
			ytdlpReason(tail, nil))
	}
	return path, nil
}

// parseYTProgress reads one line of our progress template. Anything yt-dlp does
// not know is the literal "NA", which becomes a zero.
func parseYTProgress(line string) (YTProgress, bool) {
	rest, ok := strings.CutPrefix(line, progressMarker)
	if !ok {
		return YTProgress{}, false
	}
	f := strings.Split(rest, "|")
	if len(f) < 6 {
		return YTProgress{}, false
	}
	p := YTProgress{
		Status:     strings.TrimSpace(f[0]),
		Downloaded: ytNum(f[1]),
		Total:      ytNum(f[2]),
		Speed:      ytNum(f[4]),
		ETA:        ytNum(f[5]),
	}
	if p.Total == 0 {
		// total_bytes is unknown until the server says so; the estimate is
		// what yt-dlp shows in the meantime.
		p.Total = ytNum(f[3])
	}
	if len(f) > 6 {
		switch v := strings.TrimSpace(f[6]); {
		case v == "" || v == "NA":
		case v == "none":
			p.Stage = "audio" // no video codec in this stream
		default:
			p.Stage = "video"
		}
	}
	return p, true
}

// ytNum accepts the integers and floats yt-dlp emits, and its "NA".
func ytNum(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return int64(f)
	}
	return 0
}

// ytdlpTitle picks the video's name out of the line yt-dlp prints when it
// starts, so the task is named before any bytes arrive.
func ytdlpTitle(line string) string {
	const marker = "] Destination: "
	if i := strings.Index(line, marker); i >= 0 {
		name := filepath.Base(strings.TrimSpace(line[i+len(marker):]))
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return ""
}

// ytdlpReason turns the tail of the output into one line worth showing. Its
// own ERROR line is the useful one when there is one.
func ytdlpReason(tail []string, waitErr error) string {
	for i := len(tail) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(tail[i]); strings.HasPrefix(s, "ERROR:") {
			return strings.TrimSpace(strings.TrimPrefix(s, "ERROR:"))
		}
	}
	for i := len(tail) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(tail[i]); s != "" {
			return s
		}
	}
	if waitErr != nil {
		return waitErr.Error()
	}
	return "no output"
}

// ---------- inspection ----------
// ytdlpJSON is the part of yt-dlp's dump we use.
type ytdlpJSON struct {
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
	Formats  []struct {
		FormatID   string  `json:"format_id"`
		Ext        string  `json:"ext"`
		Height     int     `json:"height"`
		TBR        float64 `json:"tbr"`
		VCodec     string  `json:"vcodec"`
		ACodec     string  `json:"acodec"`
		FormatNote string  `json:"format_note"`
	} `json:"formats"`
}

// InspectYTDLP asks yt-dlp what is on offer without downloading anything, so
// the user can pick a quality first.
func InspectYTDLP(ctx context.Context, o YTDLPOptions) (*StreamInfo, error) {
	bin := o.Binary
	if bin == "" {
		found, err := findYTDLP()
		if err != nil {
			return nil, err
		}
		bin = found
	}
	args := append(ytdlpConfigArgs(), "-J", "--no-playlist", "--no-warnings")
	for k, v := range o.Headers {
		if v == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "user-agent":
			args = append(args, "--user-agent", v)
		case "referer", "referrer":
			args = append(args, "--referer", v)
		default:
			args = append(args, "--add-header", k+":"+v)
		}
	}
	args = append(args, "--", o.URL)
	cmd := newYTDLPCmd(ctx, bin, args)
	runFromOwnFolder(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// The same two ways of being held up as a download has: when time is up
	// the whole tree goes, and something left holding the output after yt-dlp
	// has gone is not waited for beyond this.
	cmd.WaitDelay = ytdlpDrain
	release, err := startTree(cmd)
	if err != nil {
		return nil, err
	}
	err = cmd.Wait()
	release()
	// Cut loose from a leftover, yt-dlp having ended well: what it printed is
	// all there.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("yt-dlp could not read that page: %s",
				ytdlpReason(strings.Split(stderr.String(), "\n"), err))
		}
		return nil, err
	}
	var doc ytdlpJSON
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("yt-dlp returned something unreadable: %w", err)
	}
	info := &StreamInfo{Kind: "yt-dlp", Duration: doc.Duration, Best: -1, Title: doc.Title}
	seen := map[int]bool{}
	for _, f := range doc.Formats {
		// Only the video heights matter to the person choosing; yt-dlp pairs
		// each with the best audio by itself.
		if f.Height <= 0 || f.VCodec == "none" || f.VCodec == "" || seen[f.Height] {
			continue
		}
		seen[f.Height] = true
		label := strconv.Itoa(f.Height) + "p"
		if f.FormatNote != "" && !strings.EqualFold(f.FormatNote, label) {
			label += " " + f.FormatNote
		}
		if f.TBR > 0 {
			label += fmt.Sprintf(" · %.1f Mbps", f.TBR/1000)
		}
		info.Variants = append(info.Variants, VariantView{
			Index:     f.Height, // the height is the selector, not a list position
			Label:     label,
			Height:    f.Height,
			Bandwidth: int(f.TBR * 1000),
		})
	}
	// Tallest first: what a person scanning the list wants at the top.
	for i, j := 0, len(info.Variants)-1; i < j; i, j = i+1, j-1 {
		info.Variants[i], info.Variants[j] = info.Variants[j], info.Variants[i]
	}
	if len(info.Variants) > 0 {
		info.Best = info.Variants[0].Index
	}
	return info, nil
}

// ytdlpFormatFor turns a chosen height into a format selector. Asking for a
// height rather than a format id survives yt-dlp picking different streams on
// the day, and it still falls back to whatever single file exists.
func ytdlpFormatFor(height int) string {
	if height <= 0 {
		return "" // yt-dlp's own default: best video plus best audio
	}
	h := strconv.Itoa(height)
	return "bv*[height<=" + h + "]+ba/b[height<=" + h + "]/b"
}
