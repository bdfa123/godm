package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A yt-dlp download is not one file. Until it is done there are the parts of a
// stream (each with ".part" added, a ".ytdl" of notes beside it, and for a
// stream that comes in fragments one more file to a fragment), the streams that
// are whole and waiting to be merged, and the merge itself. Where those are is
// yt-dlp's to say, and with --print it says nothing but what it is asked to
// print, so godm cannot learn their names by listening. Nor can it tell them
// from a user's own files by looking in the download folder, where yt-dlp used
// to keep them next to everything else.
//
// So each download gets a folder of its own inside the download folder, and
// yt-dlp is told to keep everything it has not finished in one part of it
// (-P temp:) and to put what it has finished in another (-P home:). Removing
// the download with its files is then removing the folder, which cannot take
// anything that is not the download's. And yt-dlp never sees the download
// folder itself: its own idea of "this file is already there" is only ever
// about this download's folder, never a film another task finished, or the user
// kept, under the same name. When it is done godm moves the result out, under a
// name nobody is using, as it does for every other download.

// ytdlpPlace is the folders of one yt-dlp download.
type ytdlpPlace struct {
	root  string // everything of the download's, which is what is removed
	files string // what yt-dlp has finished: -P home
	work  string // what it has not: -P temp
}

// unsafeInName matches what is not allowed in the part of a folder name that
// comes from a task's id. Ids are made by godm, but the list they are read back
// from is a file anyone can edit, and an id is not to be able to name a place
// outside the download folder.
var unsafeInName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ytdlpPlaceFor is where the download with the given id works, in outDir.
func ytdlpPlaceFor(outDir, id string) ytdlpPlace {
	id = unsafeInName.ReplaceAllString(id, "_")
	if id == "" {
		id = "task"
	}
	// In full: yt-dlp is not started in godm's folder, and names the files it
	// makes in full, and what they are compared with has to be as well.
	if abs, err := filepath.Abs(outDir); err == nil {
		outDir = abs
	}
	root := filepath.Join(outDir, ".godm-"+id)
	return ytdlpPlace{root: root, files: filepath.Join(root, "files"), work: filepath.Join(root, "work")}
}

// discard removes the download's folder and everything in it, trying again for
// a while if something still has a file open: yt-dlp, ffmpeg or an antivirus
// scan may only let go a moment after they have been stopped. It removes
// nothing that is not a folder of its own making: a link put in its place is
// left alone.
func (p ytdlpPlace) discard() {
	if fi, err := os.Lstat(p.root); err != nil || !fi.IsDir() {
		return
	}
	removeTree(p.root, "the folder of a yt-dlp download")
}

// heldName is a name in the download folder taken for a file about to be moved
// to it.
type heldName struct {
	path    string
	release func()
}

func (c heldName) drop() {
	os.Remove(c.path) // the empty file made to hold the name
	c.release()
}

// claimName takes dst for good: it is reserved against the other downloads of
// this process, and made as an empty file, which fails if anything else made
// it first. taken says the name is somebody's already.
func claimName(dst string) (c heldName, taken bool, err error) {
	release, ok := reserveTarget(dst)
	if !ok {
		return heldName{}, true, nil
	}
	if _, err := os.Lstat(dst); err == nil {
		release()
		return heldName{}, true, nil
	}
	// A sidecar beside the name means a download of godm's is on its way to it.
	if _, err := os.Lstat(statePath(dst)); err == nil {
		release()
		return heldName{}, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		release()
		return heldName{}, false, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		release()
		if errors.Is(err, fs.ErrExist) {
			return heldName{}, true, nil
		}
		return heldName{}, false, err
	}
	f.Close()
	return heldName{path: dst, release: release}, false, nil
}

// renameWhenFree is os.Rename, tried again for a few seconds if it fails: a file
// that has just been written may still be held for a moment by whatever wrote
// it, or by a virus scan looking at what it wrote.
func renameWhenFree(from, to string) error {
	var err error
	for i := range 20 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 25 * time.Millisecond)
	}
	return err
}

// listTree lists the regular files below dir, as paths from it. The one that is
// the same file as target is not among them but named separately, as target's
// path, or "" if there is none.
func listTree(dir string, target os.FileInfo) (others []string, targetRel string, err error) {
	err = filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && name == dir {
				return filepath.SkipAll // no such folder: nothing in it
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && os.SameFile(target, info) {
			targetRel = rel
		} else {
			others = append(others, rel)
		}
		return nil
	})
	return others, targetRel, err
}

// settle moves what yt-dlp finished out of the download's folder into outDir,
// and removes the folder. path is the file yt-dlp said it made; the others are
// what went with it, if the user's own configuration asks for subtitles, a
// thumbnail or a description, and they keep their place beside it. It returns
// where the file is now.
//
// A name that is taken in outDir is not written over: the file and the ones
// with it become "name (1).ext" and so on, as a download of any other kind
// does, and they are never the file that was there.
func (p ytdlpPlace) settle(outDir, path string) (string, error) {
	if abs, err := filepath.Abs(outDir); err == nil {
		outDir = abs // where the file is said to be is not to depend on where godm is
	}
	made, err := os.Lstat(path)
	if err != nil || !made.Mode().IsRegular() {
		return "", fmt.Errorf("yt-dlp said it saved %s, but there is no such file", filepath.Base(path))
	}
	// Which file it is goes by the file and not by how its path is spelled: a
	// path can name a folder in more ways than one, and what is done to this one
	// is deleting its folder.
	rels, mainRel, err := listTree(p.files, made)
	if err != nil {
		return "", err
	}
	if mainRel == "" {
		if _, inside, err := listTree(p.root, made); err != nil || inside != "" {
			// Somewhere in the download's own folder but not where finished files
			// are put. Whatever that means, the folder is not deleted around it.
			return "", fmt.Errorf("yt-dlp said it saved %s, but not in the folder it was given for that", filepath.Base(path))
		}
		// The user's configuration has yt-dlp write somewhere of its own (an
		// output template that is a full path ignores -P). Wherever it is, it is
		// theirs to have put there, and no longer in anything godm made.
		p.discard()
		return path, nil
	}

	// The film, and what is named after it beside it, take a name together: the
	// first number for which every one of them is free. The rest are no part of
	// that, and each takes the first name that is free for it.
	stem, _ := splitExt(filepath.Base(mainRel))
	mainDir := filepath.Dir(mainRel)
	var together, apart []string
	for _, rel := range rels {
		if filepath.Dir(rel) == mainDir && strings.HasPrefix(filepath.Base(rel), stem+".") {
			together = append(together, rel)
		} else {
			apart = append(apart, rel)
		}
	}
	together = append(together, mainRel)
	// together is renamed as one, so that a player still finds the subtitles by
	// the name of the film.
	togetherAt := func(rel string, n int) string {
		dst := filepath.Join(outDir, rel)
		if n == 0 {
			return dst
		}
		if rel == mainRel {
			return numberedName(dst, n)
		}
		name := filepath.Base(rel)
		return filepath.Join(filepath.Dir(dst), fmt.Sprintf("%s (%d)%s", stem, n, name[len(stem):]))
	}

	// moves are in the order they are made, the film last.
	type move struct {
		rel  string
		name heldName
	}
	var moves []move
	dropAll := func() {
		for _, m := range moves {
			m.name.drop()
		}
		moves = nil
	}
	for n := 0; ; n++ {
		if n >= 10000 {
			dropAll()
			return "", fmt.Errorf("could not find a free name for %s", filepath.Base(path))
		}
		taken := false
		var mine []move
		for _, rel := range together {
			c, isTaken, err := claimName(togetherAt(rel, n))
			if err != nil {
				for _, m := range mine {
					m.name.drop()
				}
				dropAll()
				return "", err
			}
			if isTaken {
				taken = true
				break
			}
			mine = append(mine, move{rel, c})
		}
		if !taken {
			moves = append(moves, mine...)
			break
		}
		for _, m := range mine {
			m.name.drop()
		}
	}
	for _, rel := range apart {
		for n := 0; ; n++ {
			if n >= 10000 {
				dropAll()
				return "", fmt.Errorf("could not find a free name for %s", filepath.Base(rel))
			}
			c, isTaken, err := claimName(numberedName(filepath.Join(outDir, rel), n))
			if err != nil {
				dropAll()
				return "", err
			}
			if !isTaken {
				moves = append(moves, move{rel, c})
				break
			}
		}
	}
	// What is not named after the film goes first and the film last, so that a
	// failure in between has moved nothing that matters more than what is put back.
	slices.SortStableFunc(moves, func(a, b move) int {
		switch {
		case a.rel == mainRel:
			return 1
		case b.rel == mainRel:
			return -1
		}
		return 0
	})
	defer func() {
		for _, m := range moves {
			m.name.release()
		}
	}()

	for i, m := range moves {
		if err := renameWhenFree(filepath.Join(p.files, m.rel), m.name.path); err != nil {
			// Put back what was moved, so that running it again finds all of it,
			// and let go of the names that were held.
			for _, done := range moves[:i] {
				os.Rename(done.name.path, filepath.Join(p.files, done.rel))
			}
			for _, m := range moves[i:] {
				os.Remove(m.name.path)
			}
			return "", fmt.Errorf("could not move %s to the download folder: %w", filepath.Base(path), err)
		}
	}
	p.discard()
	return moves[len(moves)-1].name.path, nil
}
