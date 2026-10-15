package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// YouTube, and the sites like it, change what they serve all the time, and
// yt-dlp is released again to keep up with them: twelve releases in a year, now
// and then several within days. A copy that is a few months old fails on the
// sites it is wanted for, with an error that says nothing of its age. So godm
// looks after it: it can say which version is installed and which is the
// newest, update it, and when a download fails with an old copy it says so.
//
// How an update is done depends on how yt-dlp was installed, and yt-dlp's own
// README is the authority on that ("UPDATE" in the INSTALLATION section): the
// release binaries update themselves with yt-dlp -U; for pip you run the
// command you installed it with again; and for any other package manager you
// use that package manager. So godm runs -U for a standalone yt-dlp.exe,
// winget for the copy winget installed, and for the rest only tells the person
// what to run. A copy installed by a package manager that is updated by
// yt-dlp -U is a copy the package manager no longer knows the version of, and
// one that pip installed will not be updated by it at all ("You installed
// yt-dlp from a manual build or with a package manager; Use that to update").

// ---------- reasons ----------

// ytWhy is a reason godm words itself. Msg is the English of it, which is what
// anything but the page reads; Code names the reason and Arg is the one thing
// it has to say, so that the page can word it again in its own language. A
// reason with no Code is somebody else's wording (yt-dlp's, winget's) and is
// shown as it came.
type ytWhy struct{ Code, Arg, Msg string }

func (e ytWhy) Error() string { return e.Msg }

// asWhy is the reason in err, whether godm worded it or not.
func asWhy(err error) ytWhy {
	var w ytWhy
	if errors.As(err, &w) {
		return w
	}
	return ytWhy{Msg: err.Error()}
}

// ---------- versions ----------

// ytVersion is a version of yt-dlp as it writes one: the date of the release,
// year.month.day, and after it, for a release made again the same day,
// a number (2022.06.22.1), and for a nightly build the time it was made
// (2026.10.15.232837). They are compared number by number, so a build made
// later the same day is the newer.
type ytVersion []int

var ytVersionShape = regexp.MustCompile(`^(\d{4})\.(\d{2})\.(\d{2})((?:\.\d{1,9})*)$`)

// parseYTVersion reads a version. A channel in front of it ("nightly@2026...",
// as yt-dlp -v writes one) is left off. Anything that is not shaped like a date
// is not a version godm can compare: a build from a source checkout, a package
// manager's own numbering.
func parseYTVersion(s string) (ytVersion, bool) {
	s = strings.TrimSpace(s)
	if _, rest, ok := strings.Cut(s, "@"); ok {
		s = rest
	}
	m := ytVersionShape.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	var v ytVersion
	for _, part := range []string{m[1], m[2], m[3]} {
		n, _ := strconv.Atoi(part)
		v = append(v, n)
	}
	if v[1] < 1 || v[1] > 12 || v[2] < 1 || v[2] > 31 {
		return nil, false
	}
	for _, part := range strings.Split(strings.TrimPrefix(m[4], "."), ".") {
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		v = append(v, n)
	}
	return v, true
}

// compare is -1, 0 or 1 as v is older than, the same as, or newer than o. A
// version without the number after the date is the one with zero there.
func (v ytVersion) compare(o ytVersion) int {
	for i := range max(len(v), len(o)) {
		var a, b int
		if i < len(v) {
			a = v[i]
		}
		if i < len(o) {
			b = o[i]
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ytOutdated says whether have is older than newest. known is false when either
// is not a version that can be compared, and then neither answer is true: it is
// not claimed that the copy is old, and not that it is current.
func ytOutdated(have, newest string) (outdated, known bool) {
	a, ok1 := parseYTVersion(have)
	b, ok2 := parseYTVersion(newest)
	if !ok1 || !ok2 {
		return false, false
	}
	return a.compare(b) < 0, true
}

// ---------- how it was installed ----------

// The ways yt-dlp can have got onto the computer, as far as godm can tell.
const (
	ytInstWinget     = "winget"     // the copy winget installed: updated with winget
	ytInstStandalone = "standalone" // a release binary anywhere else: updated by yt-dlp -U
	ytInstScoop      = "scoop"
	ytInstChoco      = "chocolatey"
	ytInstBrew       = "brew"
	ytInstPipx       = "pipx"
	ytInstPip        = "pip"
	ytInstPackage    = "package" // a launcher or a script: pip, or something else's package
)

// ytPipCommand is what yt-dlp's wiki gives for installing and updating with pip.
const ytPipCommand = `python -m pip install -U "yt-dlp[default]"`

// ytInstall says how a yt-dlp was installed.
type ytInstall struct {
	Kind string
	// Name is how a person calls what installed it, and Command what they run to
	// update it themselves; both are empty where godm does it, and Name where
	// there is no telling.
	Name    string
	Command string
}

// godmUpdates is whether godm runs the update for this kind.
func (i ytInstall) godmUpdates() bool {
	return i.Kind == ytInstWinget || i.Kind == ytInstStandalone
}

// ytStandaloneMin is how big a file has to be to be a release binary. The
// released yt-dlp.exe carries a Python with it and is some twenty megabytes,
// and the zipapp that is the release on Linux and macOS about three. What pip
// puts on the PATH is a launcher of a hundred kilobytes on Windows and a few
// lines of script elsewhere, and the packages of other managers are shims and
// scripts as well.
const ytStandaloneMin = 1 << 20

// slashed spells a path in lower case with forward slashes, which is how the
// folders of the package managers are looked for in it whichever system the
// path came from.
func slashed(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
}

// detectYTInstall works out how the yt-dlp at bin was installed, by where it is
// and what it is. A link is followed, since winget puts its links in one folder
// and its packages in another.
func detectYTInstall(bin string) ytInstall {
	spellings := []string{slashed(bin)}
	if real, err := filepath.EvalSymlinks(bin); err == nil && real != bin {
		spellings = append(spellings, slashed(real))
	}
	has := func(parts ...string) bool {
		for _, s := range spellings {
			for _, p := range parts {
				if strings.Contains(s, p) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("/winget/packages/yt-dlp.yt-dlp_"):
		return ytInstall{Kind: ytInstWinget}
	case has("/scoop/"):
		return ytInstall{Kind: ytInstScoop, Name: "Scoop", Command: "scoop update yt-dlp"}
	case has("/chocolatey/"):
		return ytInstall{Kind: ytInstChoco, Name: "Chocolatey", Command: "choco upgrade yt-dlp"}
	case has("/cellar/", "/homebrew/", "/linuxbrew/"):
		return ytInstall{Kind: ytInstBrew, Name: "Homebrew", Command: "brew upgrade yt-dlp"}
	case has("/pipx/venvs/"):
		return ytInstall{Kind: ytInstPipx, Name: "pipx", Command: "pipx upgrade yt-dlp"}
	case has("/site-packages/"):
		return ytInstall{Kind: ytInstPip, Name: "pip", Command: ytPipCommand}
	}
	if fi, err := os.Stat(bin); err != nil || fi.Size() < ytStandaloneMin {
		// pip's launcher in a Scripts folder, a script a distribution wrote, a
		// shim: not something -U can replace.
		return ytInstall{Kind: ytInstPackage, Command: ytPipCommand}
	}
	return ytInstall{Kind: ytInstStandalone}
}

// ---------- winget ----------

// wingetScope is the scope winget is told the copy was installed in. winget
// installs a portable package for the user unless it was asked for the machine,
// and a copy in the user's folders is the first; "" leaves winget to look in
// both. Told the wrong one it finds no such package.
func wingetScope(bin string) string {
	s := slashed(bin)
	if local := slashed(os.Getenv("LOCALAPPDATA")); local != "" && strings.HasPrefix(s, local+"/") ||
		strings.Contains(s, "/appdata/local/") {
		return "user"
	}
	if strings.Contains(s, "/program files") || strings.Contains(s, "/programdata/") {
		return "machine"
	}
	return ""
}

// wingetUpgradeArgs is winget upgrading one package, without a question to
// answer: the agreements are accepted in advance, since there is nobody to
// ask, anything else it might ask is not asked (nobody reads its input, and a
// question would hold every download of yt-dlp up until the time allowed for
// an update ran out), and the package is named exactly, since winget otherwise
// takes the name for a search.
func wingetUpgradeArgs(id, scope string) []string {
	args := []string{"upgrade", "--id", id, "--exact", "--source", "winget"}
	if scope != "" {
		args = append(args, "--scope", scope)
	}
	return append(args, "--silent", "--accept-package-agreements", "--accept-source-agreements",
		"--disable-interactivity")
}

// The exit codes of winget that are told apart. They are listed in
// winget-cli's doc/windows/package-manager/winget/returnCodes.md.
const (
	wingetNothingNewer    = 0x8A15002B // APPINSTALLER_CLI_ERROR_UPDATE_NOT_APPLICABLE
	wingetNoSuchPackage   = 0x8A150014 // ..._NO_APPLICATIONS_FOUND
	wingetInUse           = 0x8A150101 // ..._INSTALL_PACKAGE_IN_USE
	wingetFileInUse       = 0x8A150103 // ..._INSTALL_FILE_IN_USE
	wingetSourceAgreement = 0x8A150046 // ..._SOURCE_AGREEMENTS_NOT_ACCEPTED
	wingetNeedsAdmin      = 0x8A150019 // ..._COMMAND_REQUIRES_ADMIN
	wingetUserScopeAdmin  = 0x8A15007D // ..._ADMIN_CONTEXT_ACTION_PROHIBITED
)

// exitCodeOf is the code a program ended with, in the unsigned form the
// documentation of winget writes them in. ok is false for an error that is not
// the program ending.
func exitCodeOf(err error) (code uint32, ok bool) {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) && ee.ExitCode() != -1 {
		return uint32(ee.ExitCode()), true
	}
	return 0, false
}

// wingetOutcome reads how a winget upgrade went. current means winget has
// nothing newer than what is installed, which is a result and no failure.
func wingetOutcome(out string, err error) (ok, current bool, reason ytWhy) {
	if err == nil {
		return true, false, ytWhy{}
	}
	code, ran := exitCodeOf(err)
	switch {
	case ran && code == wingetNothingNewer:
		return true, true, ytWhy{}
	case ran && (code == wingetInUse || code == wingetFileInUse):
		return false, false, ytWhy{"winget-inuse", "", "yt-dlp is in use by another program: close it, then try again"}
	case ran && code == wingetNoSuchPackage:
		return false, false, ytWhy{"winget-nopackage", "", "winget does not know this copy of yt-dlp as one it installed"}
	case ran && code == wingetSourceAgreement:
		return false, false, ytWhy{"winget-agreement", "",
			"winget wants its source agreement accepted first: run winget upgrade once in a terminal"}
	case ran && code == wingetNeedsAdmin:
		return false, false, ytWhy{"winget-admin", "",
			"winget needs administrator rights to update this copy of yt-dlp: update it from a terminal opened as administrator"}
	case ran && code == wingetUserScopeAdmin:
		return false, false, ytWhy{"winget-elevated", "",
			"godm is running with administrator rights, and winget will not update a copy of yt-dlp installed for one user from there: quit godm and start it again without them"}
	}
	msg := lastSaying(out)
	if msg == "" {
		msg = err.Error()
	}
	if ran {
		msg = fmt.Sprintf("%s (winget exit code %#x)", msg, code)
	}
	return false, false, ytWhy{Msg: msg}
}

// progressLine matches the lines winget draws a download with: a bar, a
// percentage, bytes of bytes.
var progressLine = regexp.MustCompile(`[█▒░▓]|^\s*[\d.]+\s*[KMG]?i?B\s*/\s*[\d.]+\s*[KMG]?i?B\s*$|^\s*\d+\s*%\s*$`)

// lastSaying is the last thing a program said that is words: winget redraws a
// spinner and a progress bar over one line with carriage returns, and the
// reason a run failed is the line after those. It is cut to one line of a
// length that fits a message, and never through a character: winget writes in
// the language of the person who uses it.
func lastSaying(out string) string {
	lines := outputLines(out)
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.Trim(s, `-\|/ `) == "" || progressLine.MatchString(s) {
			continue
		}
		if len(s) > 300 {
			s = clip(s, 300) + "…"
		}
		return s
	}
	return ""
}

// findWinget is where winget is: on the PATH as winget.exe, which Windows
// provides as an alias in the user's WindowsApps folder. It is a variable so a
// test cannot run the real one.
var findWinget = func() string {
	if p, err := lookPath("winget"); err == nil {
		return p
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		p := filepath.Join(local, "Microsoft", "WindowsApps", "winget.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// lookupTool finds a program the way godm finds any other; a variable for the
// tests, which must not depend on what the computer has installed.
var lookupTool = lookPath

// isWingetPackage says whether the program at path is in the folder winget
// keeps the package with this id in.
func isWingetPackage(path, id string) bool {
	spellings := []string{slashed(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		spellings = append(spellings, slashed(real))
	}
	for _, s := range spellings {
		if strings.Contains(s, "/winget/packages/"+strings.ToLower(id)+"_") {
			return true
		}
	}
	return false
}

// denoViaWinget is whether the Deno yt-dlp will find is one winget installed.
// YouTube needs a JavaScript runtime and yt-dlp asks for a newer Deno than it
// did before from time to time, so an update of yt-dlp that leaves an old Deno
// behind can be an update that does not help.
func denoViaWinget() bool {
	p, err := lookupTool("deno")
	return err == nil && isWingetPackage(p, "DenoLand.Deno")
}

// ---------- the newest release ----------

// ytdlpLatestURL is GitHub's answer to "what is the latest release of
// yt-dlp": the most recent one that is not a draft or a prerelease, which is
// the stable channel, the one the winget package and yt-dlp -U follow. It is a
// variable so the tests can ask a server of their own.
var ytdlpLatestURL = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"

// fetchLatestYTDLP asks GitHub for the tag of the latest release, over godm's
// usual client, so that the system's proxy is used. It is a variable so the
// tests do not reach the network.
var fetchLatestYTDLP = func(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ytdlpLatestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// GitHub turns away a request that does not say who it is.
	req.Header.Set("User-Agent", "godm/"+version)
	client := newClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", ytWhy{"gh-timeout", "", "GitHub did not answer in time"}
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", ytWhy{"gh-unreachable", "", fmt.Sprintf("could not reach GitHub: %v", err)}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0":
		w := ytWhy{"gh-limit", "", "GitHub's limit on requests from this address was reached"}
		if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Until(time.Unix(secs, 0)); wait > 0 {
				w.Arg = strconv.Itoa(int(wait/time.Minute) + 1)
				w.Msg += fmt.Sprintf("; try again in %s minutes", w.Arg)
			}
		}
		return "", w
	default:
		return "", ytWhy{"gh-status", resp.Status, "GitHub answered " + resp.Status}
	}
	var doc struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return "", ytWhy{"gh-unreadable", "", fmt.Sprintf("GitHub's answer could not be read: %v", err)}
	}
	tag := strings.TrimSpace(doc.Tag)
	if _, ok := parseYTVersion(tag); !ok {
		return "", ytWhy{"gh-tag", tag, fmt.Sprintf("GitHub names the latest release %q, which is not a yt-dlp version", tag)}
	}
	return tag, nil
}

var (
	// ytLatestTTL is how long a release found is taken to be the newest. They
	// come out a few times a month, and GitHub allows an address sixty requests
	// an hour without a key.
	ytLatestTTL = 6 * time.Hour
	// ytLatestRetry is how long a failure to find out is remembered before
	// asking again, so that a computer with no way to GitHub does not try at every
	// failed download.
	ytLatestRetry = time.Minute
	// ytFetchBudget is as long as GitHub is given to answer.
	ytFetchBudget = 10 * time.Second
	// ytVersionBudget is as long as yt-dlp is given to say its version. The
	// released yt-dlp.exe unpacks itself first, which takes a few seconds the
	// first time a virus scanner looks at it.
	ytVersionBudget = 20 * time.Second
	// ytCheckBudget bounds a whole check, the two of those side by side.
	ytCheckBudget = 30 * time.Second
	// ytHintBudget is as long as a failed download is held up to say that
	// yt-dlp is old. A version already known costs nothing.
	ytHintBudget = 4 * time.Second
	// ytUpdateTimeout bounds one program of an update.
	ytUpdateTimeout = 10 * time.Minute
	// ytResultKeep is how long the page is told how the last update went. After
	// that it is old news, and a release since then must not be hidden by it.
	ytResultKeep = 30 * time.Minute
	// ytQuitWait is as long as quitting godm waits for an update to finish.
	ytQuitWait = 2 * time.Minute
)

// ---------- not while it is in use ----------

// A running yt-dlp.exe cannot be replaced on Windows, and one that is replaced
// under a download that is only about to start would fail it. So everything
// that runs yt-dlp goes through this gate, and an update takes it whole: it is
// refused while anything runs, and downloads that come meanwhile wait for it.
type ytGate struct {
	mu       sync.Mutex
	users    int
	updating bool
	freed    chan struct{} // closed when an update ends
}

// ytdlpGate is the one gate; the program has one yt-dlp. It is a variable so a
// test can have a gate of its own.
var ytdlpGate = &ytGate{}

var (
	errYTDLPUpdating = errors.New("yt-dlp is being updated: try again in a moment")
	errYTDLPInUse    = errors.New("yt-dlp is in use")
)

// enter is for a user that can wait: it returns once no update is in the way,
// or when ctx ends.
func (g *ytGate) enter(ctx context.Context) error {
	for {
		g.mu.Lock()
		if !g.updating {
			g.users++
			g.mu.Unlock()
			return nil
		}
		if g.freed == nil {
			g.freed = make(chan struct{})
		}
		ch := g.freed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// tryEnter is for one that would rather be told.
func (g *ytGate) tryEnter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updating {
		return errYTDLPUpdating
	}
	g.users++
	return nil
}

func (g *ytGate) leave() {
	g.mu.Lock()
	if g.users > 0 {
		g.users--
	}
	g.mu.Unlock()
}

// inUse is how many are using yt-dlp now.
func (g *ytGate) inUse() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.users
}

// beginUpdate takes the gate for an update, if nobody is using yt-dlp.
func (g *ytGate) beginUpdate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updating {
		return errYTDLPUpdating
	}
	if g.users > 0 {
		return errYTDLPInUse
	}
	g.updating = true
	return nil
}

// isUpdating says whether an update has the gate.
func (g *ytGate) isUpdating() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.updating
}

// idle waits until no update has the gate, for at most max, and says whether
// that is so.
func (g *ytGate) idle(max time.Duration) bool {
	limit := time.NewTimer(max)
	defer limit.Stop()
	for {
		g.mu.Lock()
		if !g.updating {
			g.mu.Unlock()
			return true
		}
		if g.freed == nil {
			g.freed = make(chan struct{})
		}
		ch := g.freed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-limit.C:
			return false
		}
	}
}

func (g *ytGate) endUpdate() {
	g.mu.Lock()
	g.updating = false
	if g.freed != nil {
		close(g.freed)
		g.freed = nil
	}
	g.mu.Unlock()
}

// ---------- running a program ----------

// tailBuffer keeps the last of what is written to it. A program that fills a
// pipe with a progress bar for ten minutes is not to fill godm's memory, and
// what is wanted from it is the end.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailKeep = 64 << 10

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*tailKeep {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-tailKeep:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) > tailKeep {
		return string(b.buf[len(b.buf)-tailKeep:])
	}
	return string(b.buf)
}

// runCaptured runs cmd to its end and returns everything it printed, the two
// streams in the order they came. The whole tree ends with the context, as for
// a download, and nothing it leaves running is waited for beyond ytdlpDrain:
// the released yt-dlp.exe starts a command of its own to delete the old copy of
// itself once an update has replaced it. cmd must come from exec.CommandContext.
func runCaptured(cmd *exec.Cmd) (string, error) {
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = ytdlpDrain
	hideWindow(cmd)
	release, err := startTree(cmd)
	if err != nil {
		return "", err
	}
	err = cmd.Wait()
	release()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // it ended well, and what was left holding the output is not ours
	}
	return out.String(), err
}

// outputLines splits what a program printed into lines, at carriage returns
// as well: a progress bar is one line drawn many times over.
func outputLines(out string) []string {
	return strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '\r' })
}

// newWingetCmd is a variable so that no test runs winget.
var newWingetCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	// Not the folder godm happens to be in, which may be one downloads are saved to.
	cmd.Dir = os.TempDir()
	return cmd
}

// ---------- the tool ----------

// ytTool is what godm knows about the yt-dlp on this computer: its version, the
// newest release, and an update in progress. The zero value is ready.
type ytTool struct {
	mu sync.Mutex

	// The newest release: when it was learnt, or why the last attempt to learn
	// it failed. A failure leaves the answer before it where it was, so that a
	// failed download can still say its yt-dlp is old.
	latest      string
	latestAt    time.Time
	latestErr   ytWhy // Msg is empty when the last attempt did not fail
	latestErrAt time.Time
	flight      chan struct{} // closed when the request in the air is answered

	// The installed version, with what it was read from. A file that has changed
	// since is read again.
	seen    ytSeen
	seenErr ytWhy

	updating bool
	step     string // which program an update is running now
	result   *ytUpdateResult
	resultAt time.Time
}

type ytSeen struct {
	path    string
	size    int64
	mod     time.Time
	version string
}

// newest is the tag of the latest release of yt-dlp. A release learnt within
// ytLatestTTL is not asked for again unless force says so, and nor is a failure
// within ytLatestRetry; callers that come while a request is in the air wait
// for it and share its answer. When it fails the tag is the last one learnt, if
// any, with the error.
func (t *ytTool) newest(ctx context.Context, force bool) (string, error) {
	var mine chan struct{}
	for mine == nil {
		t.mu.Lock()
		now := time.Now()
		failed := t.latestErr.Msg != ""
		switch {
		// A release learnt lately is the newest, unless a look since then failed
		// and the time to try again has come: the failure is not left standing as
		// if it were the latest word, for as long as the answer before it is fresh.
		case !force && t.latest != "" && now.Sub(t.latestAt) < ytLatestTTL &&
			(!failed || now.Sub(t.latestErrAt) < ytLatestRetry):
			tag := t.latest
			t.mu.Unlock()
			return tag, nil
		case !force && failed && now.Sub(t.latestErrAt) < ytLatestRetry:
			tag, err := t.latest, t.latestErr
			t.mu.Unlock()
			return tag, err
		case t.flight != nil:
			wait := t.flight
			t.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return t.knownNewest(), ctx.Err()
			}
			force = false // the answer that came is as new as one can be
		default:
			mine = make(chan struct{})
			t.flight = mine
			t.mu.Unlock()
		}
	}

	fctx, cancel := context.WithTimeout(ctx, ytFetchBudget)
	tag, err := fetchLatestYTDLP(fctx)
	cancel()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.flight = nil
	close(mine)
	switch {
	case err == nil:
		t.latest, t.latestAt, t.latestErr = tag, time.Now(), ytWhy{}
		return tag, nil
	case ctx.Err() == nil:
		// A request that was given up on says nothing about GitHub.
		t.latestErr, t.latestErrAt = asWhy(err), time.Now()
	}
	return t.latest, err
}

// knownNewest is the newest release learnt so far, however long ago.
func (t *ytTool) knownNewest() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.latest
}

// installedVersion is the version of the yt-dlp at bin, read from it unless it
// was read from this file as it is now. force reads it again anyway. It does
// not read while an update is replacing the file.
func (t *ytTool) installedVersion(ctx context.Context, bin string, force bool) (string, error) {
	if fi, err := os.Stat(bin); err == nil && !force {
		t.mu.Lock()
		s := t.seen
		t.mu.Unlock()
		if s.version != "" && s.path == bin && s.size == fi.Size() && s.mod.Equal(fi.ModTime()) {
			return s.version, nil
		}
	}
	if err := ytdlpGate.tryEnter(); err != nil {
		return "", err
	}
	defer ytdlpGate.leave()
	return t.readVersion(ctx, bin)
}

// readVersion runs yt-dlp --version and keeps what it says. The configuration is
// ignored: nothing in it can change the version, and a fault in it could stop
// yt-dlp saying anything.
func (t *ytTool) readVersion(ctx context.Context, bin string) (string, error) {
	fi, statErr := os.Stat(bin)
	vctx, cancel := context.WithTimeout(ctx, ytVersionBudget)
	defer cancel()
	cmd := newYTDLPCmd(vctx, bin, []string{"--ignore-config", "--version"})
	runFromOwnFolder(cmd)
	cleanUp := prepareEnv(cmd)
	defer cleanUp()
	out, err := runCaptured(cmd)

	keep := func(v string) (string, error) {
		t.mu.Lock()
		t.seen, t.seenErr = ytSeen{path: bin, version: v}, ytWhy{}
		if statErr == nil {
			t.seen.size, t.seen.mod = fi.Size(), fi.ModTime()
		}
		t.mu.Unlock()
		return v, nil
	}
	fail := func(w ytWhy) (string, error) {
		t.mu.Lock()
		t.seen, t.seenErr = ytSeen{}, w
		t.mu.Unlock()
		return "", w
	}
	lines := outputLines(out)
	// A line that is not a date is taken for a version only from a yt-dlp that ended well.
	v, found := versionIn(lines, err == nil)
	switch {
	case found:
		return keep(v)
	case ctx.Err() != nil:
		return "", ctx.Err()
	case vctx.Err() != nil:
		return fail(ytWhy{"version-timeout", "", "yt-dlp did not say its version in time"})
	case err != nil:
		return fail(ytWhy{Msg: "yt-dlp --version failed: " + ytdlpReasonLine(lines, err)})
	case len(lines) == 0:
		return fail(ytWhy{Msg: "yt-dlp --version printed nothing"})
	}
	return fail(ytWhy{Msg: "yt-dlp --version said nothing but a complaint"})
}

// versionIn picks the version out of what yt-dlp --version printed. That is
// the one line that is a date; failing that, and if asked to be lenient, a
// build of someone's own: the last line that is not a complaint (the warnings
// are written to the same pipe). found is false when there is nothing that
// could be a version.
func versionIn(lines []string, lenient bool) (v string, found bool) {
	for _, line := range lines {
		if _, ok := parseYTVersion(line); ok {
			return strings.TrimSpace(line), true
		}
	}
	for i := len(lines) - 1; lenient && i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "WARNING:") && !strings.HasPrefix(line, "ERROR:") {
			return clip(line, 60), true
		}
	}
	return "", false
}

// look finds out the installed version and the newest release side by side. A
// missing yt-dlp, or one that cannot say its version, comes back as have == "".
func (t *ytTool) look(ctx context.Context, force bool) (have, newest string, haveErr, newestErr error) {
	var wg sync.WaitGroup
	if bin, err := findYTDLP(); err == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			have, haveErr = t.installedVersion(ctx, bin, force)
		}()
	} else {
		haveErr = err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		newest, newestErr = t.newest(ctx, force)
	}()
	wg.Wait()
	return
}

// outdatedNote is what a failed run of yt-dlp adds to its reason: the version
// installed and the newest, when the first is older than the second. Nothing is
// said unless both are known and the installed one is the older. It waits at
// most ytHintBudget, and the looking goes on after that in the background
// within a time of its own: the first time yt-dlp.exe is started it can take
// longer than a failure is to be held up, and a look cut short each time would
// leave the version unknown for good, and the note never given.
func (t *ytTool) outdatedNote(ctx context.Context) (have, newest string, outdated bool) {
	type looked struct {
		have, newest string
		haveErr      error
	}
	lctx, stop := context.WithTimeout(context.WithoutCancel(ctx), ytCheckBudget)
	done := make(chan looked, 1)
	go func() {
		defer stop()
		h, n, haveErr, _ := t.look(lctx, false)
		done <- looked{h, n, haveErr}
	}()
	wait := time.NewTimer(ytHintBudget)
	defer wait.Stop()
	var l looked
	select {
	case l = <-done:
	case <-wait.C:
		return "", "", false
	case <-ctx.Done():
		return "", "", false
	}
	have, newest = l.have, l.newest
	if l.haveErr != nil || have == "" || newest == "" {
		return "", "", false
	}
	// An error from GitHub with an older answer behind it is still an answer:
	// releases only come out, so the newest of an earlier look is at least that.
	outdated, known := ytOutdated(have, newest)
	return have, newest, known && outdated
}

// ---------- what the page asks about ----------

// ytStatus is what GET /api/ytdlp answers, and what a check and the start of an
// update answer with.
type ytStatus struct {
	OK    bool `json:"ok"`
	Found bool `json:"found"`
	// Path is the yt-dlp godm uses, Install how it was installed (one of the
	// ytInst constants), InstallName the name of what installed it when that is
	// not godm's to run, and Command what to run to update it.
	Path        string `json:"path,omitempty"`
	Install     string `json:"install,omitempty"`
	InstallName string `json:"install_name,omitempty"`
	Command     string `json:"command,omitempty"`
	CanUpdate   bool   `json:"can_update"`
	// DenoViaWinget says the Deno yt-dlp will find came from winget as well, and
	// so an update brings it along: which is something to update even when
	// yt-dlp itself is the newest.
	DenoViaWinget bool `json:"deno_via_winget"`
	// Version is the installed version, empty when it has not been read or could
	// not be; VersionError says why in the second case. Every error comes in
	// English, and with a code and an argument where godm worded it, so that the
	// page can word it in its own language (see ytWhy).
	Version          string `json:"version,omitempty"`
	VersionError     string `json:"version_error,omitempty"`
	VersionErrorCode string `json:"version_error_code,omitempty"`
	VersionErrorArg  string `json:"version_error_arg,omitempty"`
	// Latest is the newest release godm has learnt, which may be an old
	// answer if the last attempt failed, and then LatestError says why.
	Latest          string `json:"latest,omitempty"`
	LatestAt        string `json:"latest_at,omitempty"`
	LatestError     string `json:"latest_error,omitempty"`
	LatestErrorCode string `json:"latest_error_code,omitempty"`
	LatestErrorArg  string `json:"latest_error_arg,omitempty"`
	// Outdated is true if the installed version is older than Latest, false if it
	// is not, and null where either cannot be compared.
	Outdated *bool `json:"outdated"`
	// Busy says yt-dlp is in use, so an update would be refused.
	Busy     bool            `json:"busy"`
	Updating bool            `json:"updating"`
	Step     string          `json:"step,omitempty"`
	Result   *ytUpdateResult `json:"result,omitempty"`
}

// ytUpdateResult is how the last update went.
type ytUpdateResult struct {
	OK bool `json:"ok"`
	// From and To are the versions before and after, as read from yt-dlp.
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Changed bool   `json:"changed"`
	// Behind says a newer release is out than the one installed now: winget's
	// list follows GitHub's by a day or so.
	Behind    bool   `json:"behind"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrorArg  string `json:"error_arg,omitempty"`
	// Deno is "updated", "current" or "failed" where Deno came from winget too.
	Deno          string `json:"deno,omitempty"`
	DenoError     string `json:"deno_error,omitempty"`
	DenoErrorCode string `json:"deno_error_code,omitempty"`
	DenoErrorArg  string `json:"deno_error_arg,omitempty"`
}

// status is what is known, without asking anyone anything: the answer to a page
// that is only being opened.
func (t *ytTool) status() ytStatus {
	st := ytStatus{OK: true}
	bin, err := findYTDLP()
	var inst ytInstall
	if err == nil {
		st.Found, st.Path = true, bin
		inst = detectYTInstall(bin)
		st.Install, st.InstallName, st.CanUpdate = inst.Kind, inst.Name, inst.godmUpdates()
		if !st.CanUpdate {
			st.Command = inst.Command
		}
		st.DenoViaWinget = inst.Kind == ytInstWinget && denoViaWinget()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st.Found && t.seen.path == bin {
		st.Version = t.seen.version
	}
	if st.Found && st.Version == "" {
		st.VersionError, st.VersionErrorCode, st.VersionErrorArg = t.seenErr.Msg, t.seenErr.Code, t.seenErr.Arg
	}
	st.Latest = t.latest
	if !t.latestAt.IsZero() {
		st.LatestAt = t.latestAt.UTC().Format(time.RFC3339)
	}
	st.LatestError, st.LatestErrorCode, st.LatestErrorArg = t.latestErr.Msg, t.latestErr.Code, t.latestErr.Arg
	if outdated, known := ytOutdated(st.Version, st.Latest); known {
		st.Outdated = &outdated
	}
	st.Busy = ytdlpGate.inUse() > 0
	st.Updating, st.Step = t.updating, t.step
	if t.result != nil && time.Since(t.resultAt) < ytResultKeep {
		st.Result = t.result
	}
	return st
}

// check looks at the installed version and the newest release, using what was
// found within the last few hours unless force says to look again, and answers
// with the status. A look that is asked for forgets how the last update went;
// one that is only the dialog being opened does not, or a person who came back
// to it would be shown a newer release available and not why the update they
// had asked for left it so.
func (t *ytTool) check(ctx context.Context, force bool) ytStatus {
	t.mu.Lock()
	if t.updating {
		t.mu.Unlock()
		return t.status() // the file is being replaced: nothing to read from it
	}
	if force {
		t.result = nil
	}
	t.mu.Unlock()
	t.look(ctx, force)
	return t.status()
}

// ---------- updating ----------

// ytRefusal is an update that was not started, and why: Reason is one of
// "missing", "manual", "winget", "busy" and "updating", for the page to put in
// words of its own.
type ytRefusal struct {
	Reason string
	Msg    string
}

func (e *ytRefusal) Error() string { return e.Msg }

// ytPlan is what an update runs.
type ytPlan struct {
	bin    string
	inst   ytInstall
	winget string // the winget to run, for a copy winget installed
	scope  string
	deno   bool // and Deno is winget's too
}

// startUpdate begins updating yt-dlp in the background and returns at once, or
// says why not. The page follows it through status.
func (t *ytTool) startUpdate() error {
	// While an update is under way the file may be gone for a moment, and then
	// "yt-dlp is not there" would be the wrong answer to a second one.
	if ytdlpGate.isUpdating() {
		return &ytRefusal{"updating", "yt-dlp is already being updated"}
	}
	bin, err := findYTDLP()
	if err != nil {
		return &ytRefusal{"missing", err.Error()}
	}
	plan := ytPlan{bin: bin, inst: detectYTInstall(bin)}
	if !plan.inst.godmUpdates() {
		msg := "godm does not update a yt-dlp installed this way: update it the way it was installed"
		if plan.inst.Command != "" {
			msg += ", for example with " + plan.inst.Command
		}
		return &ytRefusal{"manual", msg}
	}
	if plan.inst.Kind == ytInstWinget {
		if plan.winget = findWinget(); plan.winget == "" {
			return &ytRefusal{"winget", "winget was not found on this computer: run " +
				"winget upgrade --id yt-dlp.yt-dlp in a terminal"}
		}
		plan.scope = wingetScope(bin)
		plan.deno = denoViaWinget()
	}
	switch err := ytdlpGate.beginUpdate(); {
	case errors.Is(err, errYTDLPUpdating):
		return &ytRefusal{"updating", "yt-dlp is already being updated"}
	case err != nil:
		return &ytRefusal{"busy", "yt-dlp is in use by a download or a page being read: " +
			"pause it or wait until it is done, then update"}
	}
	t.mu.Lock()
	t.updating, t.step, t.result = true, "yt-dlp", nil
	t.mu.Unlock()
	go t.runUpdate(plan)
	return nil
}

// runUpdate does the update, then lets everyone back in.
func (t *ytTool) runUpdate(plan ytPlan) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := t.doUpdate(ctx, plan)
	t.mu.Lock()
	t.updating, t.step, t.result, t.resultAt = false, "", &res, time.Now()
	t.mu.Unlock()
	// After the result is there to be read: a page that sees the update over
	// sees how it went.
	ytdlpGate.endUpdate()
}

func (t *ytTool) setStep(step string) {
	t.mu.Lock()
	t.step = step
	t.mu.Unlock()
}

func (t *ytTool) doUpdate(ctx context.Context, plan ytPlan) (res ytUpdateResult) {
	res.From, _ = t.readVersion(ctx, plan.bin)
	failed := func(w ytWhy) ytUpdateResult {
		res.Error, res.ErrorCode, res.ErrorArg = w.Msg, w.Code, w.Arg
		return res
	}

	switch plan.inst.Kind {
	case ytInstWinget:
		// Nothing newer than what is installed is a result and no failure, and
		// the versions read afterwards say it. Nor does it stop Deno from being
		// looked at: yt-dlp can be the newest while the Deno it wants is not.
		ok, _, why := t.winget(ctx, plan, "yt-dlp.yt-dlp")
		if !ok {
			return failed(why)
		}
		if plan.deno {
			t.setStep("Deno")
			ok, current, why := t.winget(ctx, plan, "DenoLand.Deno")
			switch {
			case !ok:
				res.Deno, res.DenoError, res.DenoErrorCode, res.DenoErrorArg = "failed", why.Msg, why.Code, why.Arg
			case current:
				res.Deno = "current"
			default:
				res.Deno = "updated"
			}
		}
	default:
		// yt-dlp -U: with the user's own configuration, for the proxy it may
		// name, like everything else godm asks of yt-dlp.
		uctx, cancel := context.WithTimeout(ctx, ytUpdateTimeout)
		cmd := newYTDLPCmd(uctx, plan.bin, append(ytdlpConfigArgs(), "-U"))
		runFromOwnFolder(cmd)
		cleanUp := prepareEnv(cmd)
		out, err := runCaptured(cmd)
		cleanUp()
		timedOut := uctx.Err() != nil
		cancel()
		if err != nil {
			log.Printf("yt-dlp -U failed: %v\n%s", err, out)
			return failed(ytUpdateFailure(out, err, timedOut))
		}
	}

	// What is there now has to say what it is. An update after which yt-dlp does
	// not run is no good news, and neither is one of a copy that did not run
	// before and still does not: the person is told, and not that all went well.
	to, err := t.readVersion(ctx, plan.bin)
	if err != nil {
		log.Printf("yt-dlp: version after the update: %v", err)
		w := asWhy(err)
		return failed(ytWhy{"after-version", w.Msg, "yt-dlp does not say its version after the update: " + w.Msg})
	}
	res.OK, res.To = true, to
	res.Changed = to != res.From
	if outdated, known := ytOutdated(to, t.knownNewest()); known && outdated {
		res.Behind = true
	}
	return res
}

// winget runs one winget upgrade of the plan and reads how it went.
func (t *ytTool) winget(ctx context.Context, plan ytPlan, id string) (ok, current bool, reason ytWhy) {
	wctx, cancel := context.WithTimeout(ctx, ytUpdateTimeout)
	defer cancel()
	cmd := newWingetCmd(wctx, plan.winget, wingetUpgradeArgs(id, plan.scope))
	out, err := runCaptured(cmd)
	ok, current, reason = wingetOutcome(out, err)
	if !ok {
		// What winget printed is more than a message has room for.
		log.Printf("winget upgrade %s: %v\n%s", id, err, out)
		if wctx.Err() != nil {
			reason = ytTimeout("timeout-winget", "winget")
		}
	}
	return ok, current, reason
}

// ytTimeout is a program of an update that did not end in the time allowed it.
func ytTimeout(code, what string) ytWhy {
	mins := strconv.Itoa(int(ytUpdateTimeout / time.Minute))
	return ytWhy{code, mins, fmt.Sprintf("%s did not finish in %s minutes", what, mins)}
}

// ytUpdateFailure is the reason yt-dlp -U gave for failing: its own ERROR
// line, as for any other failure of yt-dlp's.
func ytUpdateFailure(out string, err error, timedOut bool) ytWhy {
	if timedOut {
		return ytTimeout("timeout-update", "yt-dlp -U")
	}
	return ytWhy{Msg: ytdlpReasonLine(outputLines(out), err)}
}

// finishYTDLPUpdate is for quitting godm. Ending winget or yt-dlp -U in the
// middle of replacing yt-dlp can leave none: so a person who quits during an
// update is told it is being waited for, and godm goes when it is over, or when
// ytQuitWait is.
func (m *Manager) finishYTDLPUpdate() {
	if !ytdlpGate.isUpdating() {
		return
	}
	log.Printf("quitting waits for the update of yt-dlp to end")
	if m.announce != nil {
		m.announce(m.tr("ytdlp.quitTitle"), m.tr("ytdlp.quitText"), "")
	}
	if !ytdlpGate.idle(ytQuitWait) {
		log.Printf("the update of yt-dlp did not end in %v: quitting anyway", ytQuitWait)
	}
}

// ---------- the API ----------

// handleYTDLP answers what is known about yt-dlp without asking anyone.
func (s *server) handleYTDLP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.mgr.yt.status())
}

// handleYTDLPCheck looks at the installed version and the newest release.
// ?force=1 asks again even if the answers are recent.
func (s *server) handleYTDLPCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ytCheckBudget)
	defer cancel()
	writeJSON(w, s.mgr.yt.check(ctx, r.URL.Query().Get("force") == "1"))
}

// handleYTDLPUpdate starts an update and answers at once; the progress is in
// GET /api/ytdlp. A refusal is ok:false with a reason the page can word itself.
func (s *server) handleYTDLPUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.yt.startUpdate(); err != nil {
		var ref *ytRefusal
		if errors.As(err, &ref) {
			writeJSON(w, map[string]any{"ok": false, "reason": ref.Reason, "error": ref.Msg})
			return
		}
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, s.mgr.yt.status())
}

// ytdlpFailureText is the reason a yt-dlp job failed, with a note added when
// the copy that failed is older than the newest release.
func (m *Manager) ytdlpFailureText(err error) string {
	reason := err.Error()
	var failed *YTDLPFailedError
	if !errors.As(err, &failed) {
		return reason
	}
	have, newest, outdated := m.yt.outdatedNote(context.Background())
	if !outdated {
		return reason
	}
	return reason + " — " + m.tr("ytdlp.outdated", "have", have, "newest", newest)
}
