package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

type headerFlag map[string]string

func (h headerFlag) String() string { return "" }
func (h headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header must look like Name: value")
	}
	h[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

func main() {
	args := os.Args[1:]

	// Chrome launches the native host with the caller origin as argv[1].
	// That is our signal to speak the stdio protocol instead of the CLI.
	if len(args) > 0 && (strings.HasPrefix(args[0], "chrome-extension://") ||
		strings.HasPrefix(args[0], "moz-extension://")) {
		if err := RunNativeHost(); err != nil {
			os.Exit(1)
		}
		return
	}

	if len(args) == 0 {
		// Double-clicked in Explorer: behave like an app, not like a script
		// that printed help into a window that vanishes.
		if launchedByDoubleClick() {
			hideConsole()
			if err := cmdUI(); err != nil {
				alert("godm", "Could not start the download manager:\n\n"+err.Error())
				os.Exit(1)
			}
			return
		}
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "get":
		err = cmdGet(args[1:])
	case "daemon":
		err = cmdDaemon(args[1:])
	case "native-host":
		err = RunNativeHost()
	case "install":
		err = cmdInstall(args[1:])
	case "uninstall":
		err = cmdUninstall()
	case "status":
		err = cmdStatus()
	case "ui":
		err = cmdUI()
	case "version", "-v", "--version":
		fmt.Println("godm", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`godm ` + version + ` - segmented download manager with browser takeover

  godm get <url> [-o dir] [-n conns] [-H "Name: value"] [--name file]
      Download one URL directly. Resumes automatically if interrupted.

  godm daemon [-port 16801] [-o dir] [-parallel 3]
      Run the background service that owns all transfers.

  godm install --ext-id <extension-id>
      Register the native messaging host so the browser extension can reach us.

  godm uninstall     Remove the native messaging registration.
  godm status        Show daemon state and current tasks.
  godm ui            Open the web UI in your browser.
`)
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	outDir := fs.String("o", defaultDownloadDir(), "output directory")
	conns := fs.Int("n", 8, "number of connections")
	name := fs.String("name", "", "override filename")
	headers := headerFlag{}
	fs.Var(headers, "H", "extra request header, repeatable")

	// stdlib flag stops at the first positional argument, so "get <url> -n 8"
	// would silently ignore every flag. Hoist the flags to the front first.
	flagArgs, positional := hoistFlags(args, map[string]bool{"o": true, "n": true, "name": true, "H": true})
	fs.Parse(append(flagArgs, positional...))

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: godm get <url> [-o dir] [-n conns]")
	}
	url := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("probing %s\n", url)
	start := time.Now()
	var lastLine int

	res, err := Download(ctx, Options{
		URL:         url,
		Headers:     headers,
		OutDir:      *outDir,
		Filename:    *name,
		Connections: *conns,
		OnProgress: func(received, total int64) {
			line := renderProgress(received, total, time.Since(start))
			pad := ""
			if d := lastLine - len(line); d > 0 {
				pad = strings.Repeat(" ", d)
			}
			lastLine = len(line)
			fmt.Printf("\r%s%s", line, pad)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}
	mode := "single stream"
	if res.Segments > 1 {
		mode = fmt.Sprintf("%d segments", res.Segments)
	}
	if res.Resumed {
		mode += ", resumed"
	}
	secs := res.Elapsed.Seconds()
	if secs <= 0 {
		secs = 0.001
	}
	fmt.Printf("done  %s  %s in %s (%s/s, %s)\n",
		res.Path, humanBytes(res.Size), res.Elapsed.Round(time.Millisecond),
		humanBytes(int64(float64(res.Size)/secs)), mode)
	return nil
}

// hoistFlags splits argv into flags and positionals so they can appear in any
// order. valueFlags names the flags that consume the following argument.
func hoistFlags(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			return
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // value is inline
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return
}

func renderProgress(received, total int64, elapsed time.Duration) string {
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 0.001
	}
	rate := humanBytes(int64(float64(received)/secs)) + "/s"
	if total <= 0 {
		return fmt.Sprintf("%s  %s", humanBytes(received), rate)
	}
	pct := float64(received) / float64(total) * 100
	const width = 28
	filled := int(pct / 100 * width)
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("=", filled) + strings.Repeat(".", width-filled)
	return fmt.Sprintf("[%s] %5.1f%%  %s / %s  %s", bar, pct,
		humanBytes(received), humanBytes(total), rate)
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "loopback port")
	outDir := fs.String("o", defaultDownloadDir(), "download directory")
	parallel := fs.Int("parallel", 3, "how many downloads run at once")
	fs.Parse(args)
	return RunDaemon(*port, *outDir, *parallel)
}

// nativeManifest is the file Chrome reads to learn how to launch us.
type nativeManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	extID := fs.String("ext-id", "", "extension ID (comma separated for several)")
	fs.Parse(args)

	if *extID == "" {
		return fmt.Errorf("need --ext-id; load the extension unpacked first, then copy its ID from the extensions page")
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	var origins []string
	for _, id := range strings.Split(*extID, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		id = strings.TrimPrefix(id, "chrome-extension://")
		id = strings.TrimSuffix(id, "/")
		origins = append(origins, "chrome-extension://"+id+"/")
	}

	man := nativeManifest{
		Name:           nativeHostAppName,
		Description:    "godm download manager native host",
		Path:           exe,
		Type:           "stdio",
		AllowedOrigins: origins,
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(configDir(), nativeHostAppName+".json")
	if err := os.WriteFile(manifestPath, b, 0o644); err != nil {
		return err
	}

	if _, err := loadOrCreateToken(); err != nil {
		return err
	}

	fmt.Println("manifest:", manifestPath)
	fmt.Println("binary:  ", exe)
	for _, o := range origins {
		fmt.Println("allowed: ", o)
	}
	registered := registerNativeHost(manifestPath)
	if len(registered) == 0 {
		return fmt.Errorf("could not register with any browser (%s)", installTargetsLabel)
	}
	for _, r := range registered {
		fmt.Println("registered:", r)
	}
	fmt.Println("\nRestart the browser, then downloads will be handed to godm.")
	return nil
}

func cmdUninstall() error {
	removed := unregisterNativeHost()
	if len(removed) == 0 {
		fmt.Println("nothing was registered")
		return nil
	}
	for _, r := range removed {
		fmt.Println("removed:", r)
	}
	return nil
}

func cmdStatus() error {
	c, err := newDaemonClient()
	if err != nil {
		fmt.Println("daemon: not running")
		return nil
	}
	if err := c.ping(); err != nil {
		fmt.Println("daemon: stale port file, not responding")
		return nil
	}
	fmt.Printf("daemon: %s\n", c.base)
	tasks, err := c.tasks()
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no tasks")
		return nil
	}
	for _, t := range tasks {
		pct := ""
		if t.Size > 0 {
			pct = fmt.Sprintf(" %.1f%%", float64(t.Received)/float64(t.Size)*100)
		}
		line := fmt.Sprintf("  %-8s%s %s  %s", t.State, pct, humanBytes(t.Received), t.Filename)
		if t.Error != "" {
			line += "  <- " + t.Error
		}
		fmt.Println(line)
	}
	return nil
}

func cmdUI() error {
	c, err := ensureDaemon()
	if err != nil {
		return err
	}
	url := c.base + "/?token=" + c.token
	fmt.Println("opening", c.base)
	return openInBrowser(url)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
