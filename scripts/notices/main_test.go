package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const mitText = `MIT License

Copyright (c) 2026 Someone

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND.
`

const bsd2Text = `Copyright (c) 2015 Someone. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS".
`

const bsd3Text = `Copyright (c) 2009 The Go Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS".
`

const iscText = `ISC License

Copyright (c) 2012 Someone

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.
`

func TestIdentify(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"MIT", mitText, []string{"MIT"}},
		{"MIT in CRLF with a comment marker", "# " + strings.ReplaceAll(mitText, "\n", "\r\n# "), []string{"MIT"}},
		{"ISC", iscText, []string{"ISC"}},
		{"BSD two clause", bsd2Text, []string{"BSD-2-Clause"}},
		{"BSD three clause", bsd3Text, []string{"BSD-3-Clause"}},
		{"BSD with the advertising clause", bsd3Text + "   * All advertising materials mentioning features of this software must display this.\n", nil},
		{"Apache", "                                 Apache License\n                           Version 2.0, January 2004\n", []string{"Apache-2.0"}},
		{"MPL", "Mozilla Public License Version 2.0\n==================================\n", []string{"MPL-2.0"}},
		{"Apache and the BSD text of bundled code", "Apache License\nVersion 2.0, January 2004\n\n" + bsd3Text, []string{"Apache-2.0", "BSD-3-Clause"}},
		{"GPL, which must not pass", "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n", nil},
		{"MIT without the notice condition", strings.Replace(mitText, "The above copyright notice and this permission notice shall be included in all\ncopies or substantial portions of the Software.", "", 1), nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		if got := identify(tt.text); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: identify = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCleanText(t *testing.T) {
	got := cleanText("\xef\xbb\xbf\r\n\r\nline one  \r\nline two\t\r\n\r\n\r\n")
	if want := "line one\nline two\n"; got != want {
		t.Errorf("cleanText = %q, want %q", got, want)
	}
}

func TestEscapePath(t *testing.T) {
	if got, want := escapePath("github.com/RoaringBitmap/roaring"), "github.com/!roaring!bitmap/roaring"; got != want {
		t.Errorf("escapePath = %q, want %q", got, want)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A license in a directory that holds linked code (or that code embeds files
// from) is part of the notices; one in a directory the build never touches is
// not, and neither is a Go file whose name merely starts with "license".
func TestReadEntryFollowsTheLinkedPackages(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"LICENSE":                mitText,
		"NOTICE":                 "Attribution that the license asks to pass on.\n",
		"README.md":              "not a license\n",
		"license_test.go":        "package x\n",
		"copied/inner/LICENSE":   bsd3Text,
		"sdk/LICENSE.txt":        bsd3Text,
		"unused/LICENSE":         "Some terms nobody has taught identify() about.\n",
		"copied/inner/code.go":   "package inner\n",
		"copied/notalicense.txt": "no\n",
	})

	e, err := readEntry("example.com/m", "v1.0.0", root, []string{
		root,
		filepath.Join(root, "copied", "inner"),
		filepath.Join(root, "sdk"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range e.Files {
		names = append(names, f.Name)
	}
	wantNames := []string{"LICENSE", "NOTICE", "copied/inner/LICENSE", "sdk/LICENSE.txt"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("files = %v, want %v", names, wantNames)
	}
	if want := []string{"BSD-3-Clause", "MIT"}; !reflect.DeepEqual(e.IDs, want) {
		t.Errorf("IDs = %v, want %v", e.IDs, want)
	}
	for _, f := range e.Files {
		if f.Notice != (f.Name == "NOTICE") {
			t.Errorf("%s: Notice = %v", f.Name, f.Notice)
		}
	}
}

func TestReadEntryFailsLoudly(t *testing.T) {
	t.Run("no license file", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"README.md": "hello\n", "NOTICE": "only a notice\n"})
		_, err := readEntry("example.com/none", "v1.0.0", root, []string{root})
		if err == nil || !strings.Contains(err.Error(), "example.com/none") || !strings.Contains(err.Error(), "no license file") {
			t.Errorf("err = %v, want one saying example.com/none has no license file", err)
		}
	})
	t.Run("unrecognised license", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"LICENSE": "GNU GENERAL PUBLIC LICENSE\nVersion 3\n"})
		_, err := readEntry("example.com/gpl", "v1.0.0", root, []string{root})
		if err == nil || !strings.Contains(err.Error(), "example.com/gpl") || !strings.Contains(err.Error(), "LICENSE") {
			t.Errorf("err = %v, want one naming example.com/gpl and its LICENSE", err)
		}
	})
	t.Run("unrecognised license in a linked subdirectory", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"LICENSE": mitText, "sub/COPYING": "Do what you like, maybe.\n"})
		_, err := readEntry("example.com/sub", "v1.0.0", root, []string{filepath.Join(root, "sub")})
		if err == nil || !strings.Contains(err.Error(), "sub/COPYING") {
			t.Errorf("err = %v, want one naming sub/COPYING", err)
		}
	})
}

func TestRender(t *testing.T) {
	entries := []entry{
		{Path: "example.com/Mpl", Version: "v1.2.3", IDs: []string{"MPL-2.0"}, Files: []licenseFile{{Name: "LICENSE", Text: "MPL text\n"}}},
		{Path: "example.com/mit", Version: "v0.1.0", IDs: []string{"MIT"}, Files: []licenseFile{{Name: "LICENSE", Text: "MIT text\n"}}},
	}
	got := render(entries, "windows", []string{"amd64", "arm64"}, "go1.99")
	for _, want := range []string{
		"example.com/Mpl  v1.2.3   MPL-2.0\n",
		"example.com/mit  v0.1.0   MIT\n",
		"https://pkg.go.dev/example.com/Mpl@v1.2.3\n",
		"https://proxy.golang.org/example.com/!mpl/@v/v1.2.3.zip\n",
		"MPL text\n",
		"MIT text\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q", want)
		}
	}
	// Only the MPL module is pointed to a source; the MIT one needs no such line.
	if strings.Contains(got, "pkg.go.dev/example.com/mit") {
		t.Error("render gives a source link for a module that is not MPL-2.0")
	}
}

// Runs the real thing against this repository, so that a dependency bump that
// brings a license nobody has looked at fails here, in CI, and not on the day a
// tag is pushed.
func TestEveryLinkedModuleHasAKnownLicense(t *testing.T) {
	if testing.Short() {
		t.Skip("asks the go tool about the real module")
	}
	got, err := build("../..", "windows", []string{"amd64", "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Go standard library",
		"github.com/jchv/go-webview2 ",
		"github.com/anacrolix/torrent ",
		"golang.org/x/time ",
		"https://proxy.golang.org/github.com/anacrolix/torrent/@v/",
		"Microsoft Corporation", // the WebView2 loader's own license, found under sdk/
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the notices lack %q", want)
		}
	}
}
