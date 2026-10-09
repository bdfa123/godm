//go:build !windows

package main

import "os"

// detectSystemLang is the language of the environment godm was started in.
func detectSystemLang() string { return langFromEnv(os.Getenv) }
