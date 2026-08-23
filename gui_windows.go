//go:build windows

package main

import (
	"fmt"
	"path/filepath"

	webview2 "github.com/jchv/go-webview2"
)

// RunApp opens a real application window instead of a browser tab. The window
// hosts the same page the daemon already serves, so there is one UI to
// maintain rather than two.
//
// WebView2 is used because it ships with Windows 11 and needs no cgo; a native
// Win32 list view with progress bars would be several hundred lines of
// message-loop plumbing for a worse result.
func RunApp() error {
	c, err := ensureDaemon()
	if err != nil {
		return err
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		// Keep the browser profile beside our own config rather than polluting
		// the working directory with an EBWebView folder.
		DataPath: filepath.Join(configDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  "godm",
			Width:  1040,
			Height: 720,
			Center: true,
		},
	})
	if w == nil {
		return fmt.Errorf("could not create the window: the WebView2 runtime is missing.\n" +
			"Install it from https://go.microsoft.com/fwlink/p/?LinkId=2124703, " +
			"or run \"godm ui\" to use your browser instead")
	}
	defer w.Destroy()

	w.Navigate(c.base + "/?token=" + c.token)
	w.Run() // blocks until the window closes
	return nil
}

func guiAvailable() bool { return true }
