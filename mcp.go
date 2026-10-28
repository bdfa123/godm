package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// godm mcp is a Model Context Protocol server on stdin and stdout, which lets
// Claude (Claude Code, or the chat in the Claude desktop app) see and drive the
// downloads of the godm running on this computer: list them, add some, pause,
// start the download list, change the limits and the settings.
//
// It is a client of the daemon, as the native host is, and finds it the same
// way, by the port and token files; the token never leaves this process, so
// Claude is never given it. It does not start a daemon of its own. The Claude
// desktop app runs what it launches in its own package container, and a
// daemon started from there would keep its list somewhere other than the real
// one.

// errMCPNotRunning is what every tool says when there is no daemon to talk to.
var errMCPNotRunning = errors.New("godm is not running on this computer: nothing answers at the address in its port file. " +
	"It has to be opened first (godm.exe, or godm in the Start menu); these tools cannot start it.")

const mcpInstructions = `godm is the download manager on this computer: a segmented HTTP downloader that also takes over the browser's downloads through its Chrome extension, downloads HLS streams, video-site pages through yt-dlp, torrents and magnet links, and can play a download in mpv or VLC while it arrives. These tools talk to the godm that is already running.

Tasks are named by ids such as t1792966665951-2, which list_tasks and add_downloads return. The download list holds downloads put aside for later: each is tried briefly (about 8 MB, to check the link) and then waits until it is started, by resume, start_list, or a time of day set with update_settings; one started from the list stays on it until it is done. How many download at once is a setting (set_limits); the rest wait in line.

Links are shown without their query string, which on a file host is often a signature. Downloads added here carry no browser cookies, so a link that needs the user to be logged in can fail where the same link handed over by the browser extension works.`

// RunMCP serves MCP on stdin and stdout until the client goes away.
func RunMCP() error {
	t := &mcpTools{daemon: newDaemonClient}
	return t.server().Run(context.Background(), &mcp.StdioTransport{})
}

// mcpTools are the tools, with the way to reach the daemon, which a test
// replaces.
type mcpTools struct {
	daemon func() (*daemonClient, error)
}

// client is the daemon, asked afresh for each call: it may have been restarted,
// on another port, since the last one. Nothing answering and something
// answering no are told apart: the second is the token or port file read here
// not being the running godm's (a stale copy, say, in the package container
// the Claude desktop app runs this in), which opening godm again does not mend.
func (t *mcpTools) client() (*daemonClient, error) {
	c, err := t.daemon()
	if err != nil {
		return nil, errMCPNotRunning
	}
	if err := c.ping(); err != nil {
		var unreachable *url.Error
		if errors.As(err, &unreachable) {
			return nil, errMCPNotRunning
		}
		return nil, fmt.Errorf("godm answered but turned the request down (%v). The token or port file read from %s "+
			"does not seem to be the running godm's; quitting godm from its tray icon and opening it again writes them anew", err, configDir())
	}
	return c, nil
}

// ---------- the tools ----------

func (t *mcpTools) server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "godm", Title: "godm download manager", Version: version},
		&mcp.ServerOptions{Instructions: mcpInstructions})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "status",
		Description: "Overview of godm: how many downloads are running, waiting, on the download list, paused, needing attention or finished; the total speed; the limits; the default download folder; when the download list starts by itself; and what happens when all downloads finish. Use list_tasks for the downloads themselves.",
		Annotations: mcpRead("godm status", false),
	}, scrubbed(t.status))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List downloads with their id, name, state, size, progress, speed and time left. The views match the tabs of godm's window. Unfinished downloads come first, finished ones newest first. Use get_task for one download in detail.",
		Annotations: mcpRead("List downloads", false),
		InputSchema: mcpSchema[listTasksIn](func(p mcpProps) {
			p.enum("view", "all", "unfinished", "download_list", "needs_attention", "finished")
			p.intRange("limit", 1, 200)
		}),
	}, scrubbed(t.listTasks))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_task",
		Description: "Everything godm shows about one download: its file, link (without the query), connections, limits, times, and for a torrent what it has uploaded.",
		Annotations: mcpRead("Get a download", false),
	}, scrubbed(t.getTask))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_settings",
		Description: "godm's settings: default download folder, sorting into folders by type, keeping the computer awake, language, what a torrent does once finished, when the download list starts, and the choices for what happens when all downloads finish.",
		Annotations: mcpRead("Get settings", false),
	}, scrubbed(t.getSettings))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_stream",
		Description: "Read the qualities a stream offers without downloading it: an HLS playlist (.m3u8), or a video-site page through yt-dlp. Pass the index (HLS) or height (video site) it returns as quality to add_downloads.",
		Annotations: mcpRead("Inspect a stream", true),
		InputSchema: mcpSchema[inspectIn](func(p mcpProps) {
			p.enum("kind", "hls", "video_site")
		}),
	}, scrubbed(t.inspect))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ytdlp_status",
		Description: "Which yt-dlp godm uses for video sites, how it was installed, its version, and the newest release. check_now asks GitHub for the newest release even if godm asked recently. Use ytdlp_update to update it.",
		Annotations: mcpRead("yt-dlp status", true),
	}, scrubbed(t.ytdlpStatus))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add_downloads",
		Description: "Add downloads to godm by link: web addresses, magnet links, .torrent links, HLS playlists, or video-site pages (kind video_site, through yt-dlp). They start in turn, or with later go on the download list instead. Returns, for each link, the id it was added under or why it was refused. For a .torrent file on this computer use add_torrent_file.",
		Annotations: mcpWrite("Add downloads", false, true),
		InputSchema: mcpSchema[addIn](func(p mcpProps) {
			p.items("urls", 1, 50)
			p.enum("kind", "auto", "torrent", "hls", "video_site")
			p.intRange("connections", 1, MaxConnections)
		}),
	}, scrubbed(t.add))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add_torrent_file",
		Description: "Add a .torrent file that is on this computer, by its full path. For a link to a .torrent, or a magnet link, use add_downloads.",
		Annotations: mcpWrite("Add a .torrent file", false, false),
	}, scrubbed(t.addTorrentFile))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "pause",
		Description: "Pause downloads by id, or all that are running or waiting with all. What has been downloaded is kept; resume continues from there. A failed download waiting to be tried again is not tried again once paused, and all stops every such try.",
		Annotations: mcpWrite("Pause downloads", false, false),
		InputSchema: mcpSchema[idsOrAllIn](func(p mcpProps) {
			p.items("ids", 1, 200)
		}),
	}, scrubbed(t.pause))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "resume",
		Description: "Start or continue downloads by id: a paused or failed one continues, and one waiting on the download list is downloaded in full. all continues every paused or failed download, and tries again what was on the download list without starting it; to start the whole download list use start_list.",
		Annotations: mcpWrite("Resume downloads", false, false),
		InputSchema: mcpSchema[idsOrAllIn](func(p mcpProps) {
			p.items("ids", 1, 200)
		}),
	}, scrubbed(t.resume))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "start_list",
		Description: "Start everything waiting on the download list, oldest first. Downloads beyond the number allowed at once wait in line; they stay on the list until they are done. Returns how many were started.",
		Annotations: mcpWrite("Start the download list", false, false),
	}, scrubbed(t.startList))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "remove_tasks",
		Description: "Remove downloads from godm by id. The downloaded files stay where they are unless delete_files is true, which deletes them from the disk, finished or not, and cannot be undone; it is meant for when the user has asked for the files to be deleted.",
		Annotations: mcpWrite("Remove downloads", true, false),
		InputSchema: mcpSchema[removeIn](func(p mcpProps) {
			p.items("ids", 1, 100)
		}),
	}, scrubbed(t.remove))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_limits",
		Description: "Set how many downloads run at once, and the speed limit all downloads share, in MiB/s (0 for none). Give either or both. Use set_task_options for one download's own limit.",
		Annotations: mcpWrite("Set limits", false, false),
		InputSchema: mcpSchema[limitsIn](func(p mcpProps) {
			p.intRange("downloads_at_once", 1, maxParallel)
			p.min("max_speed_mib_per_s", 0)
			p.max("max_speed_mib_per_s", maxMiBPerSecond)
		}),
	}, scrubbed(t.setLimits))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_task_options",
		Description: "Set one download's own speed limit, in MiB/s (0 for none), and how many connections it uses. Neither applies to a torrent or a video-site download.",
		Annotations: mcpWrite("Set a download's options", false, false),
		InputSchema: mcpSchema[taskOptionsIn](func(p mcpProps) {
			p.min("max_speed_mib_per_s", 0)
			p.max("max_speed_mib_per_s", maxMiBPerSecond)
			p.intRange("connections", 1, MaxConnections)
		}),
	}, scrubbed(t.setTaskOptions))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "update_settings",
		Description: "Change godm's settings; only what is given changes. Covers the default download folder, sorting into folders by type and their names, keeping the computer awake, language, what a torrent does once finished, the time of day the download list starts by itself, and whether a download that fails is tried again. Use set_after_all for what happens when all downloads finish.",
		Annotations: mcpWrite("Change settings", false, false),
		InputSchema: mcpSchema[settingsIn](func(p mcpProps) {
			p.enum("language", "auto", "en", "zh")
			p.enum("seed_policy", "stop", "ratio", "keep")
			p.enum("on_failure", string(failGiveUp), string(failRetry), string(failForever))
			p.intRange("retry_every_minutes", 1, maxRetryEvery)
			p.intRange("retry_times", 1, maxRetryTimes)
		}),
	}, scrubbed(t.updateSettings))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_after_all",
		Description: "Choose what the computer does once all downloads have finished: nothing, sleep or shutdown. It happens once and is then forgotten, and godm warns before it shuts down. get_settings lists the choices this computer has. Whether it waits for a download that failed is the on_failure setting (update_settings). Sleep and shutdown are meant for when the user has asked for them.",
		Annotations: mcpWrite("When all downloads finish", true, true),
		InputSchema: mcpSchema[afterAllIn](func(p mcpProps) {
			p.enum("action", "nothing", "sleep", "shutdown")
		}),
	}, scrubbed(t.setAfterAll))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "change_address",
		Description: "Give a download a new link and continue it from where it stopped, for a link that has expired (a signed file-host link, say). The new link must lead to the same file, the same size.",
		Annotations: mcpWrite("Change a download's link", false, false),
	}, scrubbed(t.changeAddress))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "play",
		Description: "Open a download in the media player (mpv or VLC) on this computer: a finished one, or a video or torrent that is still downloading, which then downloads ahead of where it plays.",
		Annotations: mcpWrite("Play a download", false, false),
	}, scrubbed(t.play))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "open_folder",
		Description: "Open the folder a download is in, in Explorer, with the file selected.",
		Annotations: mcpWrite("Open a download's folder", false, false),
	}, scrubbed(t.openFolder))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ytdlp_update",
		Description: "Start updating the yt-dlp godm uses for video sites, the way it was installed (winget, or yt-dlp's own updater). It answers at once, while the update runs on for up to a few minutes; ytdlp_status says updating until it is over, and then how it went. It is refused while yt-dlp is in use.",
		Annotations: mcpWrite("Update yt-dlp", false, true),
	}, scrubbed(t.ytdlpUpdate))

	return s
}

// mcpRead and mcpWrite are the annotations a tool is shown with, which decide
// how readily a client lets it run.
func mcpRead(title string, openWorld bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &openWorld}
}

func mcpWrite(title string, destructive, openWorld bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive, OpenWorldHint: &openWorld}
}

// mcpProps is a schema's properties, for saying what a struct tag cannot: a
// choice of values, a range.
type mcpProps map[string]*jsonschema.Schema

func (p mcpProps) enum(name string, values ...string) {
	for _, v := range values {
		p[name].Enum = append(p[name].Enum, v)
	}
}

func (p mcpProps) intRange(name string, lo, hi int) {
	l, h := float64(lo), float64(hi)
	p[name].Minimum, p[name].Maximum = &l, &h
}

func (p mcpProps) min(name string, lo float64) {
	p[name].Minimum = &lo
}

// items bounds a list, and makes it a list: a Go slice is inferred as one or
// null, and null would pass for a list of nothing to act on.
func (p mcpProps) items(name string, lo, hi int) {
	p[name].Type, p[name].Types = "array", nil
	p[name].MinItems, p[name].MaxItems = &lo, &hi
}

func (p mcpProps) max(name string, hi float64) {
	p[name].Maximum = &hi
}

// mcpSchema is the input schema inferred from In, then tightened.
func mcpSchema[In any](tighten func(mcpProps)) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	tighten(mcpProps(s.Properties))
	return s
}

// ---------- what tools show of a download ----------

// mcpTask is a download as list_tasks and get_task show it: in words and
// units a person reads, with the id needed to act on it.
type mcpTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Kind     string `json:"kind,omitempty"`
	OnList   bool   `json:"on_download_list,omitempty"`
	FromList bool   `json:"started_from_list,omitempty"`
	InLine   int    `json:"place_in_line,omitempty"`
	Size     string `json:"size,omitempty"`
	Done     string `json:"downloaded,omitempty"`
	Speed    string `json:"speed,omitempty"`
	Left     string `json:"time_left,omitempty"`
	Error    string `json:"error,omitempty"`
	Path     string `json:"path,omitempty"`
	// RetryAt is when a failed download is tried again by itself, and
	// Retries how many tries again in a row have got it no further.
	RetryAt string `json:"tried_again_at,omitempty"`
	Retries int    `json:"tries_again_so_far,omitempty"`
}

// mcpState is the state in one word, with the ones godm's window spells out
// separately: a try, a torrent waiting for its file list, one seeding.
func mcpState(v TaskView) string {
	switch {
	case v.Later && v.State == StateRunning:
		return "trying"
	case v.Later && v.State == StateQueued:
		return "waiting_to_be_tried"
	case v.Kind == "bt" && v.State == StateRunning && v.Stage == "metadata":
		return "fetching_metadata"
	case v.Kind == "bt" && v.State == StateDone && v.Stage == "seeding":
		return "seeding"
	}
	return string(v.State)
}

func briefTask(v TaskView, inLine int) mcpTask {
	b := mcpTask{
		ID: v.ID, Name: v.Filename, State: mcpState(v), Kind: v.Kind,
		OnList: v.Later, FromList: v.FromList && !v.Later, InLine: inLine,
		Error: scrubLinks(v.Error), Path: v.Path,
	}
	if b.Name == "" {
		b.Name = linkShown(v.URL)
	}
	if isFailure(v.State) {
		if !v.RetryAt.IsZero() {
			b.RetryAt = v.RetryAt.Local().Format("2006-01-02 15:04:05")
		}
		b.Retries = v.Retries
	}
	if v.Size > 0 {
		b.Size = humanBytes(v.Size)
		b.Done = fmt.Sprintf("%s (%.1f%%)", humanBytes(v.Received), float64(v.Received)/float64(v.Size)*100)
	} else if v.Received > 0 {
		b.Done = humanBytes(v.Received)
	}
	if v.State == StateRunning && v.Speed > 0 {
		b.Speed = humanBytes(v.Speed) + "/s"
		// Past a hundred days it is no estimate worth giving, and far enough
		// past it no longer fits in a Duration.
		if left := float64(v.Size-v.Received) / float64(v.Speed); v.Size > v.Received && left < 100*24*3600 {
			b.Left = (time.Duration(left) * time.Second).Round(time.Second).String()
		}
	}
	return b
}

// linkInText finds the links in a sentence, such as the error net/http writes,
// which quotes the address it was asked for in full.
var linkInText = regexp.MustCompile(`(?i)\b(?:https?|magnet):[^\s"'<>]+`)

// scrubLinks is text with every link in it shown as linkShown shows one, so
// that an error never carries a signature the tools would not show otherwise.
func scrubLinks(text string) string {
	return linkInText.ReplaceAllStringFunc(text, func(link string) string {
		if shown := linkShown(link); shown != "" {
			return shown
		}
		return "(link)"
	})
}

// scrubbed is h with every link in an error it returns scrubbed. Errors carry
// what the daemon or a server said, and that can quote a signed link.
func scrubbed[In any](h mcp.ToolHandlerFor[In, any]) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, out, err := h(ctx, req, in)
		if err != nil {
			err = errors.New(scrubLinks(err.Error()))
		}
		return res, out, err
	}
}

// linkShown is a link as the tools show it: a web address without its query,
// which on a file host is often a signature, and a magnet link by its hash
// alone.
func linkShown(raw string) string {
	if ih, ok := magnetInfoHash(raw); ok {
		return "magnet:?xt=urn:btih:" + ih.HexString()
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// queueOrder is where each waiting download is in line, from 1, as godm's
// window counts it: those in the queue, not those waiting for a try, by when
// they were added.
func queueOrder(tasks []TaskView) map[string]int {
	var waiting []TaskView
	for _, v := range tasks {
		if v.State == StateQueued && !v.Later {
			waiting = append(waiting, v)
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].AddedAt.Before(waiting[j].AddedAt) })
	at := make(map[string]int, len(waiting))
	for i, v := range waiting {
		at[v.ID] = i + 1
	}
	return at
}

// inView is whether a download is in one of the views list_tasks offers, which
// are the tabs of godm's window.
func inView(v TaskView, view string) bool {
	attention := v.State == StateNeedsRefresh || v.State == StateAwaitingRefresh || v.State == StateError
	switch view {
	case "unfinished":
		return (v.State == StateRunning || v.State == StateQueued || v.State == StatePaused) && !v.Later
	case "download_list":
		return (v.Later || v.FromList) && v.State != StateDone
	case "needs_attention":
		return attention
	case "finished":
		return v.State == StateDone
	}
	return true
}

// ---------- reading ----------

type tasksAnswer struct {
	Tasks      []TaskView `json:"tasks"`
	Limit      int        `json:"limit"`
	AfterAll   string     `json:"after_all"`
	SpeedLimit int64      `json:"speed_limit"`
	ListAt     string     `json:"list_at"`
	ListDaily  bool       `json:"list_daily"`
	ListNext   time.Time  `json:"list_next"`
}

func (t *mcpTools) tasks(c *daemonClient) (*tasksAnswer, error) {
	var a tasksAnswer
	if err := c.do(http.MethodGet, "/api/tasks", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (t *mcpTools) status(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	a, err := t.tasks(c)
	if err != nil {
		return nil, nil, err
	}
	var ping struct {
		Version string `json:"version"`
	}
	var settings struct {
		OutDir string `json:"out_dir"`
	}
	if err := c.do(http.MethodGet, "/api/ping", nil, &ping); err != nil {
		return nil, nil, err
	}
	if err := c.do(http.MethodGet, "/api/settings", nil, &settings); err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	var speed int64
	for _, v := range a.Tasks {
		counts[mcpBucket(v)]++
		if v.State == StateRunning {
			speed += v.Speed
		}
	}
	out := map[string]any{
		"version":           ping.Version,
		"downloads":         counts,
		"total_speed":       humanBytes(speed) + "/s",
		"downloads_at_once": a.Limit,
		"speed_limit":       mcpSpeed(a.SpeedLimit),
		"download_folder":   settings.OutDir,
		"after_all":         a.AfterAll,
	}
	if a.ListAt != "" {
		out["download_list_starts"] = map[string]any{"at": a.ListAt, "every_day": a.ListDaily, "next": a.ListNext.Local().Format("2006-01-02 15:04")}
	}
	return nil, out, nil
}

// mcpBucket is the heading status counts a download under.
func mcpBucket(v TaskView) string {
	switch {
	case v.State == StateDone:
		return "finished"
	case v.State == StateNeedsRefresh || v.State == StateAwaitingRefresh || v.State == StateError:
		return "needs_attention"
	case v.Later && v.State == StateRunning:
		return "being_tried"
	case v.Later || v.State == StateListed:
		return "on_download_list"
	case v.State == StateRunning:
		return "downloading"
	case v.State == StateQueued:
		return "waiting_in_line"
	}
	return string(v.State)
}

func mcpSpeed(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "none"
	}
	return humanBytes(bytesPerSecond) + "/s"
}

type listTasksIn struct {
	View  string `json:"view,omitempty" jsonschema:"Which downloads: all (the default), unfinished, download_list, needs_attention or finished"`
	Limit int    `json:"limit,omitempty" jsonschema:"At most this many, 30 if not given"`
}

func (t *mcpTools) listTasks(ctx context.Context, _ *mcp.CallToolRequest, in listTasksIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	a, err := t.tasks(c)
	if err != nil {
		return nil, nil, err
	}
	view, limit := in.View, in.Limit
	if view == "" {
		view = "all"
	}
	if limit <= 0 {
		limit = 30
	}
	var shown []TaskView
	for _, v := range a.Tasks {
		if inView(v, view) {
			shown = append(shown, v)
		}
	}
	// What is still to do first, in the order it was added; then what is
	// done, the most recent first.
	sort.SliceStable(shown, func(i, j int) bool {
		di, dj := shown[i].State == StateDone, shown[j].State == StateDone
		switch {
		case di != dj:
			return dj
		case di:
			return shown[i].EndedAt.After(shown[j].EndedAt)
		}
		return shown[i].AddedAt.Before(shown[j].AddedAt)
	})
	total := len(shown)
	if len(shown) > limit {
		shown = shown[:limit]
	}
	line := queueOrder(a.Tasks)
	list := make([]mcpTask, 0, len(shown))
	for _, v := range shown {
		list = append(list, briefTask(v, line[v.ID]))
	}
	out := map[string]any{"view": view, "total": total, "tasks": list}
	if total > len(list) {
		out["note"] = fmt.Sprintf("Showing %d of %d. Ask for a narrower view, or a larger limit.", len(list), total)
	}
	return nil, out, nil
}

type idIn struct {
	ID string `json:"id" jsonschema:"The download's id, from list_tasks"`
}

// task finds one download, or says it is not there.
func (t *mcpTools) task(c *daemonClient, id string) (TaskView, []TaskView, error) {
	a, err := t.tasks(c)
	if err != nil {
		return TaskView{}, nil, err
	}
	for _, v := range a.Tasks {
		if v.ID == id {
			return v, a.Tasks, nil
		}
	}
	return TaskView{}, nil, fmt.Errorf("there is no download with id %q; list_tasks gives the ids", id)
}

func (t *mcpTools) getTask(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	v, all, err := t.task(c, in.ID)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{
		"download":    briefTask(v, queueOrder(all)[v.ID]),
		"link":        linkShown(v.URL),
		"added":       v.AddedAt.Local().Format("2006-01-02 15:04"),
		"resumable":   v.Resumable,
		"speed_limit": mcpSpeed(v.SpeedLimit),
	}
	if v.Referrer != "" {
		out["from_page"] = linkShown(v.Referrer)
	}
	if !v.EndedAt.IsZero() {
		out["finished"] = v.EndedAt.Local().Format("2006-01-02 15:04")
	}
	switch v.Kind {
	case "bt":
		out["peers"], out["seeds"] = v.Active, v.Conns
		if v.Uploaded > 0 {
			out["uploaded"] = humanBytes(v.Uploaded)
		}
	case "yt-dlp":
		if v.Quality != "" {
			out["quality"] = v.Quality
		}
	case "hls":
		out["segments"] = fmt.Sprintf("%d of %d", v.SegDone, v.SegTotal)
		if v.Quality != "" {
			out["quality"] = v.Quality
		}
	default:
		out["connections"] = map[string]any{"in_use": v.Active, "allowed": v.Conns, "chosen": v.ConnsWanted}
		if v.ServerCap > 0 {
			out["connections"].(map[string]any)["server_allows"] = v.ServerCap
		}
	}
	if v.TrySpeed > 0 {
		out["try_speed"] = humanBytes(v.TrySpeed) + "/s"
	}
	return nil, out, nil
}

func (t *mcpTools) getSettings(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	var settings, seeding map[string]any
	if err := c.do(http.MethodGet, "/api/settings", nil, &settings); err != nil {
		return nil, nil, err
	}
	if err := c.do(http.MethodGet, "/api/seeding", nil, &seeding); err != nil {
		return nil, nil, err
	}
	delete(settings, "ok")
	settings["seed_policy"], settings["seed_policy_choices"] = seeding["policy"], seeding["choices"]
	return nil, settings, nil
}

type inspectIn struct {
	URL  string `json:"url" jsonschema:"The playlist or page to read"`
	Kind string `json:"kind" jsonschema:"hls for an .m3u8 playlist, video_site for a page yt-dlp reads"`
}

func (t *mcpTools) inspect(ctx context.Context, _ *mcp.CallToolRequest, in inspectIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	info, err := c.inspect(jobRequest{URL: in.URL, Kind: mcpKind(in.Kind)})
	if err != nil {
		return nil, nil, err
	}
	return nil, info, nil
}

type ytdlpStatusIn struct {
	CheckNow bool `json:"check_now,omitempty" jsonschema:"Ask GitHub for the newest release now"`
}

func (t *mcpTools) ytdlpStatus(ctx context.Context, _ *mcp.CallToolRequest, in ytdlpStatusIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if in.CheckNow {
		// The daemon gives a check up to 30 seconds, reading the version
		// (a first run of yt-dlp unpacks itself) and asking GitHub.
		slow := *c
		slow.http = &http.Client{Timeout: 40 * time.Second}
		err = slow.do(http.MethodPost, "/api/ytdlp/check?force=1", nil, &out)
	} else {
		err = c.do(http.MethodGet, "/api/ytdlp", nil, &out)
	}
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// ---------- where things may go ----------

// checkLocalPath refuses a path on another computer, which Windows would offer
// the user's credentials to as soon as it was touched (a prompt-injected
// \\server\share is the oldest trick there is), and a relative one, which
// would be read from wherever godm happens to be running.
func checkLocalPath(p string) error {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return fmt.Errorf("%s is on another computer; only folders and files on this one can be used", p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s is not a full path; give one like D:\\Downloads", p)
	}
	return nil
}

// checkFolder refuses a folder to save downloads in unless it is a local full
// path outside the folders of the system and of programs. Windows runs what is
// in some of those (the Startup folder is under AppData), and a download put
// there by a Claude a web page has talked round would run at the next sign-in:
// far more than adding a download. The temporary folder is no such place.
func checkFolder(dir string) error {
	if err := checkLocalPath(dir); err != nil {
		return err
	}
	clean := strings.ToLower(filepath.Clean(dir))
	within := func(root string) bool {
		root = strings.ToLower(filepath.Clean(root))
		return clean == root || strings.HasPrefix(clean, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
	}
	if within(os.TempDir()) {
		return nil
	}
	for _, env := range []string{"APPDATA", "LOCALAPPDATA", "ProgramData", "SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if root := os.Getenv(env); root != "" && within(root) {
			return fmt.Errorf("%s is inside %s, a folder of the system's or of programs', which these tools do not save downloads in", dir, root)
		}
	}
	return nil
}

// ---------- adding ----------

type addIn struct {
	URLs        []string `json:"urls" jsonschema:"The links, one download each"`
	Folder      string   `json:"folder,omitempty" jsonschema:"Full path of a folder on this computer to save in, not one of the system's or of programs'; godm's default folder if not given"`
	Later       bool     `json:"later,omitempty" jsonschema:"Put them on the download list instead of starting them"`
	Filename    string   `json:"filename,omitempty" jsonschema:"The file name to save as; only with a single link"`
	Kind        string   `json:"kind,omitempty" jsonschema:"auto (the default) tells by the link; torrent for a .torrent link that does not end in .torrent; hls for a playlist; video_site for a page yt-dlp downloads"`
	Quality     *int     `json:"quality,omitempty" jsonschema:"For hls the index, for video_site the picture height, from inspect_stream; the best if not given"`
	Connections int      `json:"connections,omitempty" jsonschema:"Connections for each ordinary download; godm's default if not given"`
}

// mcpKind is the kind a tool's word stands for, in the daemon's words.
func mcpKind(kind string) string {
	switch kind {
	case "torrent":
		return "bt"
	case "hls":
		return "hls"
	case "video_site":
		return "yt-dlp"
	}
	return ""
}

// mcpAdded is how one link went: the id it was added under, or why it was not.
type mcpAdded struct {
	Link  string `json:"link"`
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

func (t *mcpTools) add(ctx context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, any, error) {
	if in.Filename != "" && len(in.URLs) != 1 {
		return nil, nil, errors.New("filename can only be given with a single link")
	}
	if in.Folder != "" {
		if err := checkFolder(in.Folder); err != nil {
			return nil, nil, err
		}
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	// Each link goes on its own, the way the browser's do, so that what is
	// said about each is said against that link; a .torrent is fetched before
	// the answer, so a link godm cannot get at fails now rather than later.
	results := make([]mcpAdded, 0, len(in.URLs))
	added := 0
	for _, u := range in.URLs {
		item := map[string]any{
			"url": strings.TrimSpace(u), "out_dir": in.Folder, "later": in.Later,
			"filename": in.Filename, "kind": mcpKind(in.Kind), "connections": in.Connections,
		}
		if in.Quality != nil {
			item["variant"] = *in.Quality
		}
		var out struct {
			ID string `json:"id"`
		}
		r := mcpAdded{Link: linkShown(u)}
		if err := c.do(http.MethodPost, "/api/download", item, &out); err != nil {
			r.Error = scrubLinks(err.Error())
		} else {
			r.ID = out.ID
			added++
		}
		results = append(results, r)
	}
	if added == 0 && len(results) == 1 {
		return nil, nil, errors.New(results[0].Error)
	}
	return nil, map[string]any{"added": added, "results": results}, nil
}

type torrentFileIn struct {
	Path   string `json:"path" jsonschema:"Full path of the .torrent file"`
	Folder string `json:"folder,omitempty" jsonschema:"Full path of a folder on this computer to save in, not one of the system's or of programs'; godm's default folder if not given"`
	Later  bool   `json:"later,omitempty" jsonschema:"Put it on the download list instead of starting it"`
}

func (t *mcpTools) addTorrentFile(ctx context.Context, _ *mcp.CallToolRequest, in torrentFileIn) (*mcp.CallToolResult, any, error) {
	// Only a .torrent is read, and only to hand to godm, which reads it as one
	// or refuses it: this is no way to read just any file.
	if checkLocalPath(in.Path) != nil || !strings.EqualFold(filepath.Ext(in.Path), ".torrent") {
		return nil, nil, errors.New("path must be the full path of a .torrent file on this computer")
	}
	if in.Folder != "" {
		if err := checkFolder(in.Folder); err != nil {
			return nil, nil, err
		}
	}
	fi, err := os.Stat(in.Path)
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() || fi.Size() > maxTorrentFile {
		return nil, nil, fmt.Errorf("%s is not a .torrent file godm can take", in.Path)
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return nil, nil, err
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	if in.Folder != "" {
		q.Set("out_dir", in.Folder)
	}
	if in.Later {
		q.Set("later", "1")
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.send(http.MethodPost, "/api/torrent?"+q.Encode(), data, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"id": out.ID}, nil
}

// ---------- acting on downloads ----------

type idsOrAllIn struct {
	IDs []string `json:"ids,omitempty" jsonschema:"The downloads' ids, from list_tasks"`
	All bool     `json:"all,omitempty" jsonschema:"All of them instead"`
}

// each runs an action that takes one id on every id given, and says how each
// went, what failed among what went through.
func (t *mcpTools) each(c *daemonClient, path string, ids []string, ok, notOK string) map[string]string {
	done := make(map[string]string, len(ids))
	for _, id := range ids {
		var out struct {
			OK bool `json:"ok"`
		}
		switch err := c.do(http.MethodPost, path+"id="+url.QueryEscape(id), nil, &out); {
		case err != nil:
			done[id] = "failed: " + scrubLinks(err.Error())
		case out.OK:
			done[id] = ok
		default:
			done[id] = notOK
		}
	}
	return done
}

func (t *mcpTools) idsOrAll(in idsOrAllIn, path, allPath string) (*mcp.CallToolResult, any, error) {
	if in.All == (len(in.IDs) > 0) {
		return nil, nil, errors.New("give either ids or all, not both and not neither")
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	if in.All {
		if err := c.do(http.MethodPost, allPath, nil, nil); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"all": "ok"}, nil
	}
	return nil, t.each(c, path+"?", in.IDs, "ok", "not done: no such download, or not in a state for it"), nil
}

func (t *mcpTools) pause(ctx context.Context, _ *mcp.CallToolRequest, in idsOrAllIn) (*mcp.CallToolResult, any, error) {
	return t.idsOrAll(in, "/api/pause", "/api/pause-all")
}

func (t *mcpTools) resume(ctx context.Context, _ *mcp.CallToolRequest, in idsOrAllIn) (*mcp.CallToolResult, any, error) {
	return t.idsOrAll(in, "/api/resume", "/api/resume-all")
}

func (t *mcpTools) startList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Started int `json:"started"`
	}
	if err := c.do(http.MethodPost, "/api/list/start", nil, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"started": out.Started}, nil
}

type removeIn struct {
	IDs         []string `json:"ids" jsonschema:"The downloads' ids, from list_tasks"`
	DeleteFiles bool     `json:"delete_files,omitempty" jsonschema:"Also delete the downloaded files from the disk"`
}

func (t *mcpTools) remove(ctx context.Context, _ *mcp.CallToolRequest, in removeIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	path := "/api/remove?"
	if in.DeleteFiles {
		path += "delete=1&"
	}
	return nil, t.each(c, path, in.IDs, "removed", "not removed: no such download"), nil
}

type limitsIn struct {
	DownloadsAtOnce *int     `json:"downloads_at_once,omitempty" jsonschema:"How many downloads run at the same time"`
	MaxSpeed        *float64 `json:"max_speed_mib_per_s,omitempty" jsonschema:"The speed limit all downloads share, in MiB/s; 0 for none"`
}

func (t *mcpTools) setLimits(ctx context.Context, _ *mcp.CallToolRequest, in limitsIn) (*mcp.CallToolResult, any, error) {
	if in.DownloadsAtOnce == nil && in.MaxSpeed == nil {
		return nil, nil, errors.New("give downloads_at_once, max_speed_mib_per_s or both")
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{}
	if in.DownloadsAtOnce != nil {
		var got struct {
			Limit int `json:"limit"`
		}
		if err := c.do(http.MethodPost, "/api/limit?n="+strconv.Itoa(*in.DownloadsAtOnce), nil, &got); err != nil {
			return nil, nil, err
		}
		out["downloads_at_once"] = got.Limit
	}
	if in.MaxSpeed != nil {
		var got struct {
			SpeedLimit int64 `json:"speed_limit"`
		}
		if err := c.do(http.MethodPost, "/api/speed?n="+mibPerSecond(*in.MaxSpeed), nil, &got); err != nil {
			return nil, nil, err
		}
		out["speed_limit"] = mcpSpeed(got.SpeedLimit)
	}
	return nil, out, nil
}

// maxMiBPerSecond is the highest speed limit the tools take, 1 TiB/s: a limit
// in name only, and far from where bytes per second would overflow.
const maxMiBPerSecond = 1 << 20

// mibPerSecond is a speed in MiB/s as the daemon takes it, bytes per second. A
// limit too small to count is the smallest there is, not none at all, which is
// what 0 means.
func mibPerSecond(v float64) string {
	n := int64(min(v, maxMiBPerSecond) * (1 << 20))
	if v > 0 && n < 1 {
		n = 1
	}
	return strconv.FormatInt(n, 10)
}

type taskOptionsIn struct {
	ID          string   `json:"id" jsonschema:"The download's id, from list_tasks"`
	MaxSpeed    *float64 `json:"max_speed_mib_per_s,omitempty" jsonschema:"This download's own speed limit, in MiB/s; 0 for none"`
	Connections *int     `json:"connections,omitempty" jsonschema:"How many connections it uses"`
}

func (t *mcpTools) setTaskOptions(ctx context.Context, _ *mcp.CallToolRequest, in taskOptionsIn) (*mcp.CallToolResult, any, error) {
	if in.MaxSpeed == nil && in.Connections == nil {
		return nil, nil, errors.New("give max_speed_mib_per_s, connections or both")
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	id := url.QueryEscape(in.ID)
	out := map[string]any{}
	if in.MaxSpeed != nil {
		var got struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			SpeedLimit int64  `json:"speed_limit"`
		}
		if err := c.do(http.MethodPost, "/api/speed?id="+id+"&n="+mibPerSecond(*in.MaxSpeed), nil, &got); err != nil {
			return nil, nil, err
		}
		if !got.OK {
			return nil, nil, errors.New(got.Error)
		}
		out["speed_limit"] = mcpSpeed(got.SpeedLimit)
	}
	if in.Connections != nil {
		var got struct {
			OK          bool   `json:"ok"`
			Error       string `json:"error"`
			Connections int    `json:"connections"`
			ServerCap   int    `json:"server_cap"`
		}
		if err := c.do(http.MethodPost, "/api/connections?id="+id+"&n="+strconv.Itoa(*in.Connections), nil, &got); err != nil {
			return nil, nil, err
		}
		if !got.OK {
			return nil, nil, errors.New(got.Error)
		}
		out["connections"] = got.Connections
		if got.ServerCap > 0 {
			out["server_allows"] = got.ServerCap
		}
	}
	return nil, out, nil
}

type settingsIn struct {
	DownloadDir   *string           `json:"download_dir,omitempty" jsonschema:"Full path of the default download folder, on this computer and not one of the system's or of programs'; empty to go back to the one godm was started with"`
	SortByType    *bool             `json:"sort_by_type,omitempty" jsonschema:"Put downloads in folders by type (videos, music, programs and so on) inside the download folder"`
	Folders       map[string]string `json:"folders,omitempty" jsonschema:"New names for the folders by type, by kind as get_settings lists them"`
	KeepAwake     *bool             `json:"keep_awake,omitempty" jsonschema:"Keep the computer from sleeping while something downloads"`
	Language      *string           `json:"language,omitempty" jsonschema:"The language of godm's window and notices: auto, en or zh"`
	SeedPolicy    *string           `json:"seed_policy,omitempty" jsonschema:"What a finished torrent does: stop uploading, upload until a ratio of 1, or keep uploading"`
	ListStartTime *string           `json:"list_start_time,omitempty" jsonschema:"The time of day the download list starts by itself, HH:MM in 24 hours; empty to stop"`
	ListDaily     *bool             `json:"list_daily,omitempty" jsonschema:"Start the download list at that time every day, rather than once"`
	OnFailure     *string           `json:"on_failure,omitempty" jsonschema:"What a download that fails or whose link expires does next: give_up leaves it failed, and a sleep or shutdown set with set_after_all goes ahead without it; retry tries it again every retry_every_minutes, up to retry_times times without getting further, then gives up; forever goes on trying, and a sleep or shutdown waits for it"`
	RetryEvery    *int              `json:"retry_every_minutes,omitempty" jsonschema:"Minutes between tries again"`
	RetryTimes    *int              `json:"retry_times,omitempty" jsonschema:"How many tries again that get no further before retry gives up"`
}

func (t *mcpTools) updateSettings(ctx context.Context, _ *mcp.CallToolRequest, in settingsIn) (*mcp.CallToolResult, any, error) {
	if in.DownloadDir != nil && *in.DownloadDir != "" {
		if err := checkFolder(*in.DownloadDir); err != nil {
			return nil, nil, err
		}
	}
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	u := settingsUpdate{
		KeepAwake: in.KeepAwake, SortByType: in.SortByType, Folders: in.Folders, Language: in.Language,
		DownloadDir: in.DownloadDir, ListAt: in.ListStartTime, ListDaily: in.ListDaily,
		OnFailure: in.OnFailure, RetryEvery: in.RetryEvery, RetryTimes: in.RetryTimes,
	}
	var out map[string]any
	if err := c.do(http.MethodPost, "/api/settings", u, &out); err != nil {
		return nil, nil, err
	}
	delete(out, "ok")
	if in.SeedPolicy != nil {
		var seeding map[string]any
		if err := c.do(http.MethodPost, "/api/seeding?policy="+url.QueryEscape(*in.SeedPolicy), nil, &seeding); err != nil {
			return nil, nil, err
		}
		out["seed_policy"] = seeding["policy"]
	}
	return nil, out, nil
}

type afterAllIn struct {
	Action string `json:"action" jsonschema:"nothing, sleep or shutdown"`
}

func (t *mcpTools) setAfterAll(ctx context.Context, _ *mcp.CallToolRequest, in afterAllIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		AfterAll string `json:"after_all"`
	}
	if err := c.do(http.MethodPost, "/api/settings", settingsUpdate{AfterAll: &in.Action}, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"after_all": out.AfterAll}, nil
}

type addressIn struct {
	ID  string `json:"id" jsonschema:"The download's id, from list_tasks"`
	URL string `json:"url" jsonschema:"The new link to the same file"`
}

func (t *mcpTools) changeAddress(ctx context.Context, _ *mcp.CallToolRequest, in addressIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	return t.okOrError(c, "/api/address?id="+url.QueryEscape(in.ID), map[string]string{"url": in.URL}, "the download goes on from the new link")
}

func (t *mcpTools) play(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	return t.okOrError(c, "/api/play?id="+url.QueryEscape(in.ID), nil, "the player is opening")
}

func (t *mcpTools) openFolder(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	return t.okOrError(c, "/api/open?id="+url.QueryEscape(in.ID), nil, "the folder is open")
}

// okOrError posts to an address that answers {"ok":true} or {"ok":false,
// "error":...}, and turns the second into an error.
func (t *mcpTools) okOrError(c *daemonClient, path string, body any, said string) (*mcp.CallToolResult, any, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.do(http.MethodPost, path, body, &out); err != nil {
		return nil, nil, err
	}
	if !out.OK {
		return nil, nil, errors.New(out.Error)
	}
	return nil, map[string]string{"result": said}, nil
}

func (t *mcpTools) ytdlpUpdate(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := t.client()
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if err := c.do(http.MethodPost, "/api/ytdlp/update", nil, &out); err != nil {
		return nil, nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		if msg, _ := out["error"].(string); msg != "" {
			return nil, nil, errors.New(msg)
		}
	}
	return nil, out, nil
}
