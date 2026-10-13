package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodList is a saved list with one half-finished download and settings that
// are not the defaults, so that losing either shows.
const goodList = `{"version":1,"limit":5,"notify":false,"speed_limit":2097152,` +
	`"tasks":[{"view":{"id":"t1","url":"https://example.test/big.iso","filename":"big.iso","state":"paused","size":100,"received":50},` +
	`"req":{"url":"https://example.test/big.iso"},"out_dir":"D:\\Downloads"}],` +
	`"settings":{"keep_awake":true}}`

// wrongTypeList is goodList as a build that kept the limit as text would have
// written it.
var wrongTypeList = []byte(strings.Replace(goodList, `"limit":5`, `"limit":"5"`, 1))

// unreadableLists are the ways a task list has been seen to go bad: a field of
// another type from a different build, a write cut short, the zeros NTFS
// leaves when the power goes between a rename and its data, and nothing.
var unreadableLists = map[string][]byte{
	"a field of another type": wrongTypeList,
	"cut short":               []byte(goodList[:len(goodList)/2]),
	"zero filled":             make([]byte, 300),
	"empty":                   {},
}

// holdsCopy reports whether some file in dir has exactly these bytes, and
// which.
func holdsCopy(t *testing.T, dir string, want []byte) (string, bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && bytes.Equal(b, want) && !e.IsDir() {
			return e.Name(), true
		}
	}
	return "", false
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// stored is a manager whose list lives in dir, with what it tells the user
// collected in notices as "title | text | what a click opens".
func stored(t *testing.T, dir string) (m *Manager, notices *[]string) {
	t.Helper()
	m = NewManager(dir, 2)
	m.store = filepath.Join(dir, "tasks.json")
	notices = new([]string)
	m.announce = func(title, text, open string) {
		*notices = append(*notices, title+" | "+text+" | "+open)
	}
	return m, notices
}

func writeStore(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hasGoodList checks that the manager holds what goodList describes.
func hasGoodList(t *testing.T, m *Manager) {
	t.Helper()
	tasks := m.List()
	if len(tasks) != 1 || tasks[0].ID != "t1" || tasks[0].Filename != "big.iso" || tasks[0].State != StatePaused || tasks[0].Received != 50 {
		t.Errorf("the tasks came back as %+v, want the one half-finished big.iso", tasks)
	}
	if m.SpeedLimit() != 2<<20 || m.Notifications() {
		t.Errorf("the settings came back as speed limit %d, notifications %v; want %d and off", m.SpeedLimit(), m.Notifications(), 2<<20)
	}
}

// A list that cannot be read is all there is of the user's queue. The first
// save after it, which any change at all brings on, must not be the end of it.
func TestAnUnreadableTaskListIsNotOverwritten(t *testing.T) {
	for name, bad := range unreadableLists {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store := writeStore(t, dir, "tasks.json", bad)
			m, _ := stored(t, dir)
			err := m.load()
			t.Logf("load: %v; tasks restored: %d", err, len(m.List()))
			if err == nil {
				t.Errorf("load said nothing was wrong")
			}

			m.SetNotifications(true) // any change marks the list for saving
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			kept, ok := holdsCopy(t, dir, bad)
			if !ok {
				now, _ := os.ReadFile(store)
				t.Fatalf("the unreadable list is gone. The folder holds %v and tasks.json is now: %s", filesIn(t, dir), now)
			}
			if !strings.HasPrefix(kept, "tasks.json.bad-") {
				t.Errorf("the unreadable list is kept as %q, want a dated name beside tasks.json", kept)
			}
			if err == nil || !strings.Contains(err.Error(), kept) {
				t.Errorf("the error does not say where the file went: %v", err)
			}
			// What is there now can be read, so the next start is a quiet one.
			m2, notices := stored(t, dir)
			if err := m2.load(); err != nil {
				t.Errorf("the list written afterwards cannot be read either: %v", err)
			}
			m2.announceLoad()
			if len(*notices) != 0 {
				t.Errorf("the next start still complains: %v", *notices)
			}
		})
	}
}

// Every save leaves the list it replaced as a backup, and that is what an
// unreadable list is replaced with: one save short of everything.
func TestAnUnreadableListIsReplacedByTheBackup(t *testing.T) {
	for name, bad := range unreadableLists {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeStore(t, dir, "tasks.json", bad)
			writeStore(t, dir, "tasks.json.bak", []byte(goodList))
			m, notices := stored(t, dir)
			if err := m.load(); err == nil || !strings.Contains(err.Error(), "the last backup was restored") {
				t.Errorf("load: %v", err)
			}
			hasGoodList(t, m)
			if !m.dirty.Load() {
				t.Errorf("nothing asks for a readable list to be written back")
			}

			kept, ok := holdsCopy(t, dir, bad)
			if !ok {
				t.Fatalf("the unreadable list was not kept; the folder holds %v", filesIn(t, dir))
			}
			m.announceLoad()
			want := "The task list could not be read | The last backup was restored.\nThe unreadable file is kept as " +
				kept + ". | " + filepath.Join(dir, kept)
			if len(*notices) != 1 || (*notices)[0] != want {
				t.Errorf("the user was told %q\nwant %q", *notices, want)
			}
			m.announceLoad()
			if len(*notices) != 1 {
				t.Errorf("the user was told twice: %q", *notices)
			}

			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			m2, _ := stored(t, dir)
			if err := m2.load(); err != nil {
				t.Fatal(err)
			}
			hasGoodList(t, m2)
		})
	}
}

// One field of another type is all that is wrong with such a file, and
// encoding/json has read the rest by the time it complains. With no backup to
// do better, the rest is what the user gets, and the file is still set aside.
func TestAFieldOfAnotherTypeCostsOnlyThatField(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir, "tasks.json", wrongTypeList)
	m, notices := stored(t, dir)
	if err := m.load(); err == nil || !strings.Contains(err.Error(), "what could be read of it was restored") {
		t.Errorf("load: %v", err)
	}
	hasGoodList(t, m)
	if m.Limit() != 2 {
		t.Errorf("the limit that could not be read came back as %d, want the 2 the manager started with", m.Limit())
	}
	kept, ok := holdsCopy(t, dir, wrongTypeList)
	if !ok {
		t.Fatalf("the original was not kept; the folder holds %v", filesIn(t, dir))
	}
	m.announceLoad()
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "What could be read of it was restored.\nThe unreadable file is kept as "+kept) {
		t.Errorf("the user was told %q", *notices)
	}

	// A backup that reads cleanly is one save behind and whole, which is
	// better than the newer file with a hole in it.
	dir = t.TempDir()
	writeStore(t, dir, "tasks.json", []byte(strings.Replace(string(wrongTypeList), "big.iso", "other.iso", -1)))
	writeStore(t, dir, "tasks.json.bak", []byte(goodList))
	m, _ = stored(t, dir)
	if err := m.load(); err == nil || !strings.Contains(err.Error(), "the last backup was restored") {
		t.Errorf("load with a clean backup: %v", err)
	}
	hasGoodList(t, m)

	// And a backup with the same fault is no better than the file, which is
	// the newer of the two.
	dir = t.TempDir()
	writeStore(t, dir, "tasks.json", wrongTypeList)
	writeStore(t, dir, "tasks.json.bak", []byte(strings.Replace(string(wrongTypeList), "big.iso", "older.iso", -1)))
	m, _ = stored(t, dir)
	if err := m.load(); err == nil || !strings.Contains(err.Error(), "what could be read of it was restored") {
		t.Errorf("load with a backup that has the same fault: %v", err)
	}
	hasGoodList(t, m)
}

// With nothing to fall back on godm starts empty, as it did, but the file is
// kept and the user hears of it instead of finding out from an empty window.
func TestAListWithNothingReadableStartsEmptyAndSaysSo(t *testing.T) {
	for _, lang := range []string{langEN, langZH} {
		dir := t.TempDir()
		bad := unreadableLists["cut short"]
		writeStore(t, dir, "tasks.json", bad)
		writeStore(t, dir, "tasks.json.bak", unreadableLists["zero filled"])
		m, notices := stored(t, dir)
		if err := m.load(); err == nil || !strings.Contains(err.Error(), "starting with an empty list") {
			t.Errorf("load: %v", err)
		}
		if n := len(m.List()); n != 0 {
			t.Errorf("%d tasks came out of a file with none readable", n)
		}
		kept, ok := holdsCopy(t, dir, bad)
		if !ok {
			t.Fatalf("the unreadable list was not kept; the folder holds %v", filesIn(t, dir))
		}

		// The switch for notices about downloads does not silence this one.
		m.SetNotifications(false)
		setLanguage(t, m, lang)
		m.announceLoad()
		want := "The task list could not be read | godm started with an empty list.\nThe unreadable file is kept as " + kept + "."
		if lang == langZH {
			want = "无法读取任务列表 | godm 已从空列表开始。\n无法读取的文件已另存为 " + kept + "。"
		}
		if len(*notices) != 1 || !strings.HasPrefix((*notices)[0], want) {
			t.Errorf("%s: the user was told %q\nwant %q", lang, *notices, want)
		}
	}
}

// The list being replaced becomes tasks.json.bak, nothing else is left lying
// about, and a start with both files in place is an ordinary one.
func TestSaveKeepsTheListItReplacesAsABackup(t *testing.T) {
	dir := t.TempDir()
	store := writeStore(t, dir, "tasks.json", []byte(goodList))
	m, notices := stored(t, dir)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	hasGoodList(t, m)

	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(backupPath(store)); string(b) != goodList {
		t.Errorf("after the first save the backup holds %q, want the list that was replaced", b)
	}
	first, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}

	m.SetLimit(7)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(backupPath(store)); !bytes.Equal(b, first) {
		t.Errorf("after the second save the backup is not the list the first one wrote")
	}
	if b, _ := os.ReadFile(store); !bytes.Contains(b, []byte(`"limit":7`)) {
		t.Errorf("the list on disk does not have the change: %s", b)
	}
	if got := filesIn(t, dir); len(got) != 2 {
		t.Errorf("the folder holds %v, want only the list and its backup", got)
	}

	m2, _ := stored(t, dir)
	if err := m2.load(); err != nil || m2.Limit() != 7 {
		t.Errorf("the next start: err %v, limit %d", err, m2.Limit())
	}
	m.announceLoad()
	m2.announceLoad()
	if len(*notices) != 0 {
		t.Errorf("an ordinary start told the user %q", *notices)
	}
}

// Between moving the old list to the backup and putting the new one in place
// there is no tasks.json. A save that stops there, because the disk is full or
// the power goes, must cost that one save and nothing else.
func TestASaveCutOffHalfwayCostsOnlyThatSave(t *testing.T) {
	dir := t.TempDir()
	store := writeStore(t, dir, "tasks.json", []byte(goodList))
	m, _ := stored(t, dir)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}

	saved := writeList
	writeList = func(string, []byte) error { return errors.New("disk full") }
	m.SetLimit(7)
	err := m.save()
	// A second try finds no list to move to the backup; the backup it has is
	// the good one and has to stay.
	err2 := m.save()
	writeList = saved
	if err == nil || err2 == nil {
		t.Fatalf("the saves that could not write reported %v and %v", err, err2)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no tasks.json after the cut-off save, stat says %v", err)
	}
	if b, _ := os.ReadFile(backupPath(store)); string(b) != goodList {
		t.Fatalf("the backup holds %q, want the last list that was saved whole", b)
	}

	m2, notices := stored(t, dir)
	if err := m2.load(); err != nil {
		t.Errorf("load after a cut-off save: %v", err)
	}
	hasGoodList(t, m2)
	m2.announceLoad()
	if len(*notices) != 0 {
		t.Errorf("nothing was damaged, yet the user was told %q", *notices)
	}
	if !m2.dirty.Load() {
		t.Errorf("nothing asks for tasks.json to be written back")
	}
	if err := m2.save(); err != nil {
		t.Fatal(err)
	}
	m3, _ := stored(t, dir)
	if err := m3.load(); err != nil {
		t.Fatal(err)
	}
	hasGoodList(t, m3)
}

// When the backup is all there is and it cannot be read, it is the file to
// keep.
func TestABackupThatIsAllThereIsAndUnreadableIsSetAsideToo(t *testing.T) {
	dir := t.TempDir()
	bad := unreadableLists["cut short"]
	writeStore(t, dir, "tasks.json.bak", bad)
	m, notices := stored(t, dir)
	if err := m.load(); err == nil {
		t.Errorf("load said nothing was wrong")
	}
	m.SetNotifications(true)
	for i := 0; i < 3; i++ { // enough saves to have gone through the backup
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
	}
	kept, ok := holdsCopy(t, dir, bad)
	if !ok || !strings.HasPrefix(kept, "tasks.json.bak.bad-") {
		t.Fatalf("the unreadable backup is kept as %q (found: %v); the folder holds %v", kept, ok, filesIn(t, dir))
	}
	m.announceLoad()
	if len(*notices) != 1 || !strings.Contains((*notices)[0], kept) {
		t.Errorf("the user was told %q", *notices)
	}

	// A backup with one field of another type still has its tasks in it.
	dir = t.TempDir()
	writeStore(t, dir, "tasks.json.bak", wrongTypeList)
	m, _ = stored(t, dir)
	if err := m.load(); err == nil || !strings.Contains(err.Error(), "what could be read of the last backup was restored") {
		t.Errorf("load: %v", err)
	}
	hasGoodList(t, m)
}

// A first run has neither file and nothing to say.
func TestAFirstRunHasNothingToRestoreAndNothingToSay(t *testing.T) {
	dir := t.TempDir()
	m, notices := stored(t, dir)
	if err := m.load(); err != nil {
		t.Errorf("load on a first run: %v", err)
	}
	m.announceLoad()
	if len(*notices) != 0 || len(m.List()) != 0 || m.dirty.Load() {
		t.Errorf("a first run told the user %q, has %d tasks, wants a save: %v", *notices, len(m.List()), m.dirty.Load())
	}
	if got := filesIn(t, dir); len(got) != 0 {
		t.Errorf("a first run left %v behind", got)
	}
}

// Two lists set aside within the same second each keep a name of their own.
func TestListsSetAsideInTheSameSecondDoNotReplaceEachOther(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for _, content := range []string{"first", "second", "third"} {
		p := writeStore(t, dir, "tasks.json", []byte(content))
		to, err := setAside(p)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, to)
	}
	for i, content := range []string{"first", "second", "third"} {
		if b, err := os.ReadFile(names[i]); err != nil || string(b) != content {
			t.Errorf("%s holds %q (%v), want %q; the names were %v", filepath.Base(names[i]), b, err, content, names)
		}
	}
}
