// Command notices writes THIRD_PARTY_NOTICES.txt: the licenses of the Go
// modules that end up inside godm.exe.
//
// godm.exe is one static binary, so everything it imports is redistributed
// with it. MIT, BSD, ISC and Apache-2.0 all require their notice to travel
// with binaries, and MPL-2.0 (the anacrolix BitTorrent modules) requires
// telling recipients where the source of those files can be had. Hand-kept
// lists go stale the day a dependency is bumped, so this reads the list from
// the Go tool, the license texts from the module cache, and gives up loudly
// when it meets a module whose license it cannot put a name to.
//
// scripts/build-release.ps1 runs it for every release:
//
//	go run ./scripts/notices -o THIRD_PARTY_NOTICES.txt
//
// It is stdlib-only and lives in its own directory so it is never part of the
// godm binary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

func main() {
	out := flag.String("o", "THIRD_PARTY_NOTICES.txt", "file to write")
	goos := flag.String("goos", "windows", "GOOS the release is built for")
	goarch := flag.String("goarch", "amd64,arm64", "comma-separated GOARCH values the release is built for")
	flag.Parse()

	text, err := build(".", *goos, strings.Split(*goarch, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	// Nothing is written unless the whole file could be made, so a failed run
	// never leaves a half-finished file that looks like a good one.
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	fmt.Printf("notices: wrote %s\n", *out)
}

// module is what "go list" says about a module a package came from.
type module struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *module
}

// linked is a module and the directories the build takes from it: those of
// its packages and of the files embedded into them. The directories matter
// because a module can keep a different license in a subdirectory (a package
// copied in from another project, say), and that license then covers what is
// linked from there.
type linked struct {
	module
	pkgDirs map[string]bool
}

// licenseFile is one license (or NOTICE) file belonging to a module.
type licenseFile struct {
	Name   string // path inside the module, with forward slashes
	Text   string
	Notice bool
	// IDs are the SPDX identifiers found in the text. A NOTICE file is not a
	// license, so it has none.
	IDs []string
}

// entry is one piece of software to give notice for.
type entry struct {
	Path    string
	Version string
	Files   []licenseFile
	IDs     []string // every license named in Files, sorted
}

// build makes the whole text of the notices file for the module in dir.
func build(dir, goos string, goarches []string) (string, error) {
	mods := map[string]*linked{}
	for _, goarch := range goarches {
		// A module needed by only one architecture still ships in that
		// architecture's zip, and the one file serves them all.
		if err := linkedModules(dir, goos, strings.TrimSpace(goarch), mods); err != nil {
			return "", err
		}
	}

	var entries []entry
	for _, m := range mods {
		root, version := m.Dir, m.Version
		if m.Replace != nil {
			root, version = m.Replace.Dir, m.Replace.Version
		}
		if root == "" {
			return "", fmt.Errorf("%s %s is not in the module cache (run go mod download)", m.Path, version)
		}
		var pkgDirs []string
		for d := range m.pkgDirs {
			pkgDirs = append(pkgDirs, d)
		}
		e, err := readEntry(m.Path, version, root, pkgDirs)
		if err != nil {
			return "", err
		}
		entries = append(entries, e)
	}

	// The Go runtime and standard library are linked in as well and are under
	// the Go license, which asks the same of a binary.
	std, err := goEntry(dir)
	if err != nil {
		return "", err
	}
	entries = append(entries, std)

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].Version < entries[j].Version
	})

	out := render(entries, goos, goarches, std.Version)
	if !utf8.ValidString(out) {
		return "", fmt.Errorf("a license file is not valid UTF-8; the notices would not read right in a text editor")
	}
	return out, nil
}

// linkedModules adds the modules that supply packages to the godm binary for
// one target to mods. Asking go list for the dependencies of the main package
// (and not "all") leaves out test-only modules and the ones only another OS
// needs.
func linkedModules(dir, goos, goarch string, mods map[string]*linked) error {
	cmd := exec.Command("go", "list", "-deps", "-json=ImportPath,Dir,EmbedFiles,Module", ".")
	cmd.Dir = dir
	// The release is built with cgo off, which also changes which packages are
	// linked, so list it the same way.
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go list: %w", err)
	}
	dec := json.NewDecoder(pipe)
	for {
		var pkg struct {
			ImportPath string
			Dir        string
			EmbedFiles []string
			Module     *module
		}
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			cmd.Wait()
			return fmt.Errorf("reading go list output: %w", err)
		}
		// Standard library packages have no module; godm's own packages are
		// covered by its own LICENSE.
		if pkg.Module == nil || pkg.Module.Main {
			continue
		}
		key := pkg.Module.Path + "@" + pkg.Module.Version
		m := mods[key]
		if m == nil {
			m = &linked{module: *pkg.Module, pkgDirs: map[string]bool{}}
			mods[key] = m
		}
		m.pkgDirs[pkg.Dir] = true
		// A file embedded into the package is in the binary too, and its
		// license sits beside it: go-webview2 carries Microsoft's
		// WebView2Loader.dll with the license in sdk/.
		for _, f := range pkg.EmbedFiles {
			m.pkgDirs[filepath.Dir(filepath.Join(pkg.Dir, filepath.FromSlash(f)))] = true
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list for %s/%s: %w\n%s", goos, goarch, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// goEntry is the Go distribution, whose LICENSE sits at the top of GOROOT.
func goEntry(dir string) (entry, error) {
	cmd := exec.Command("go", "env", "GOROOT", "GOVERSION")
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return entry{}, fmt.Errorf("go env: %w", err)
	}
	// One value per line; GOROOT may well contain spaces.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		return entry{}, fmt.Errorf("go env printed %q, expected GOROOT and GOVERSION", string(raw))
	}
	return readEntry("Go standard library", strings.TrimSpace(lines[1]), strings.TrimSpace(lines[0]), nil)
}

// licenseName matches the file names license texts are kept under. NOTICE is
// here because Apache-2.0 asks for its contents to be passed on too.
var licenseName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|unlicense|notice)([-_. ].*)?$`)

// readEntry collects the license files of the module at root: those at its top
// and those in the directory of any linked package or in a directory above one.
func readEntry(path, version, root string, pkgDirs []string) (entry, error) {
	e := entry{Path: path, Version: version}
	ids := map[string]bool{}
	for _, rel := range licenseDirs(root, pkgDirs) {
		names, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return entry{}, err
		}
		for _, de := range names {
			name := de.Name()
			if !de.Type().IsRegular() || !licenseName.MatchString(name) || strings.HasSuffix(strings.ToLower(name), ".go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, rel, name))
			if err != nil {
				return entry{}, err
			}
			f := licenseFile{
				Name:   filepath.ToSlash(filepath.Join(rel, name)),
				Text:   cleanText(string(raw)),
				Notice: strings.EqualFold(strings.SplitN(name, ".", 2)[0], "notice"),
			}
			if !f.Notice {
				f.IDs = identify(f.Text)
				if len(f.IDs) == 0 {
					return entry{}, fmt.Errorf("%s %s: %s is not a license this tool recognises; teach identify() about it, or read it and decide by hand", path, version, f.Name)
				}
				for _, id := range f.IDs {
					ids[id] = true
				}
			}
			e.Files = append(e.Files, f)
		}
	}
	if len(ids) == 0 {
		return entry{}, fmt.Errorf("%s %s has no license file in %s; its terms cannot be passed on, so decide by hand whether it can be used", path, version, root)
	}
	for id := range ids {
		e.IDs = append(e.IDs, id)
	}
	sort.Strings(e.IDs)
	return e, nil
}

// licenseDirs lists, relative to the module root, the directories to look for
// license files in: the root first, then each directory that holds a linked
// package or lies between one and the root.
func licenseDirs(root string, pkgDirs []string) []string {
	set := map[string]bool{".": true}
	for _, d := range pkgDirs {
		rel, err := filepath.Rel(root, d)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for rel != "." {
			set[rel] = true
			rel = filepath.Dir(rel)
		}
	}
	var dirs []string
	for rel := range set {
		if rel != "." {
			dirs = append(dirs, rel)
		}
	}
	sort.Strings(dirs)
	return append([]string{"."}, dirs...)
}

// cleanText makes a license file safe to paste into the notices: no byte order
// mark, LF line endings, no trailing blank lines.
func cleanText(s string) string {
	const byteOrderMark = "\xef\xbb\xbf"
	s = strings.TrimPrefix(s, byteOrderMark)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n") + "\n"
}

// squash lowercases text and reduces everything between words to one space, so
// that a phrase matches however the file wraps its lines or comments them out.
func squash(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// identify names the licenses a text contains, as SPDX identifiers. It looks
// for the operative sentences of the licenses godm's dependencies use and knows
// nothing else; a license it does not know comes back empty and stops the
// build, which is the point.
func identify(text string) []string {
	n := " " + squash(text) + " "
	has := func(phrase string) bool {
		return strings.Contains(n, " "+squash(phrase)+" ")
	}
	var ids []string

	if has("Apache License Version 2.0, January 2004") {
		ids = append(ids, "Apache-2.0")
	}
	if has("Mozilla Public License Version 2.0") {
		ids = append(ids, "MPL-2.0")
	}
	if has("Permission is hereby granted, free of charge, to any person obtaining a copy") &&
		has("The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software") {
		ids = append(ids, "MIT")
	}
	if has("Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted") ||
		has("Permission to use, copy, modify, and distribute this software for any purpose with or without fee is hereby granted") {
		ids = append(ids, "ISC")
	}
	if has("Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met") &&
		has("Redistributions of source code must retain the above copyright notice") &&
		has("Redistributions in binary form must reproduce the above copyright notice") &&
		!has("All advertising materials mentioning features") {
		if has("endorse or promote products derived from this software") {
			ids = append(ids, "BSD-3-Clause")
		} else {
			ids = append(ids, "BSD-2-Clause")
		}
	}
	sort.Strings(ids)
	return ids
}

// render lays the notices out: what this is, a table, then the texts.
func render(entries []entry, goos string, goarches []string, goVersion string) string {
	var b strings.Builder
	rule := strings.Repeat("=", 78)
	thin := strings.Repeat("-", 78)

	b.WriteString("Third-party notices for godm\n")
	b.WriteString(rule + "\n\n")
	b.WriteString("godm itself is under the MIT license (the LICENSE file next to this one).\n")
	b.WriteString("godm.exe is a single static program, so the software listed here is inside\n")
	b.WriteString("it, and the licenses below ask for these notices to go with it.\n\n")
	b.WriteString("scripts/notices makes this list again for every release, from the Go modules\n")
	fmt.Fprintf(&b, "linked into the %s builds (%s) by %s.\n\n", goos, strings.Join(trimAll(goarches), ", "), goVersion)

	// The summary table.
	wp, wv := len("Module"), len("Version")
	for _, e := range entries {
		wp = max(wp, len(e.Path))
		wv = max(wv, len(e.Version))
	}
	fmt.Fprintf(&b, "Summary (%d components)\n%s\n", len(entries), thin)
	fmt.Fprintf(&b, "%-*s  %-*s  %s\n", wp, "Module", wv, "Version", "License")
	for _, e := range entries {
		fmt.Fprintf(&b, "%-*s  %-*s  %s\n", wp, e.Path, wv, e.Version, strings.Join(e.IDs, ", "))
	}
	b.WriteString("\nWhere a module lists more than one license, it has code under each of them.\n\n")

	// MPL-2.0 does not ask for its text to be repeated so much as for the
	// recipient to be told where the source is.
	var mpl []entry
	for _, e := range entries {
		for _, id := range e.IDs {
			if id == "MPL-2.0" {
				mpl = append(mpl, e)
			}
		}
	}
	if len(mpl) > 0 {
		b.WriteString("Source code of the MPL-2.0 modules\n" + thin + "\n")
		b.WriteString("These modules are under the Mozilla Public License 2.0, which entitles you\n")
		b.WriteString("to the source of the files from them that godm.exe contains. They are used\n")
		b.WriteString("exactly as published, without changes. The source of the version in this\n")
		b.WriteString("build can be had from:\n\n")
		for _, e := range mpl {
			fmt.Fprintf(&b, "  %s %s\n", e.Path, e.Version)
			fmt.Fprintf(&b, "    https://pkg.go.dev/%s@%s\n", e.Path, e.Version)
			fmt.Fprintf(&b, "    https://proxy.golang.org/%s/@v/%s.zip\n", escapePath(e.Path), escapePath(e.Version))
		}
		b.WriteString("\n")
	}

	// The texts.
	for _, e := range entries {
		b.WriteString(rule + "\n")
		fmt.Fprintf(&b, "%s %s\n", e.Path, e.Version)
		fmt.Fprintf(&b, "License: %s\n", strings.Join(e.IDs, ", "))
		for _, f := range e.Files {
			b.WriteString(thin + "\n")
			// With one file there is nothing to tell apart, except that a
			// NOTICE is not a license and should say so.
			if len(e.Files) > 1 || f.Notice {
				fmt.Fprintf(&b, "[%s]\n\n", f.Name)
			}
			b.WriteString(f.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func trimAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// escapePath is how the module proxy spells upper-case letters in a URL: "!"
// and the lower-case letter, because it may be served from a case-insensitive
// file system.
func escapePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
