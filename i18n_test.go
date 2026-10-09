package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// Whatever language the machine running the tests is set to, they run in
// English: the other tests look for English words, and which language "auto"
// comes to must not depend on who is running them.
func init() { systemLang = func() string { return langEN } }

// asIfTheSystemSpeaks makes "auto" come to lang for the rest of the test.
func asIfTheSystemSpeaks(t *testing.T, lang string) {
	t.Helper()
	was := systemLang
	systemLang = func() string { return lang }
	t.Cleanup(func() { systemLang = was })
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func setLanguage(t *testing.T, m *Manager, lang string) {
	t.Helper()
	if err := m.UpdateSettings(settingsUpdate{Language: &lang}); err != nil {
		t.Fatal(err)
	}
}

// ---------- which language ----------

func TestLanguageFromTheNamesSystemsUse(t *testing.T) {
	for tag, want := range map[string]string{
		"zh": langZH, "zh-CN": langZH, "zh_CN.UTF-8": langZH, "zh-Hans-CN": langZH, "ZH-tw": langZH, " zh-HK ": langZH,
		"en": langEN, "en_US.UTF-8": langEN, "de-DE": langEN, "ja_JP": langEN, "C": langEN, "POSIX": langEN, "": langEN,
	} {
		if got := langFromTag(tag); got != want {
			t.Errorf("langFromTag(%q) = %q, want %q", tag, got, want)
		}
	}
	// Windows names a language with a number: the low ten bits are the language
	// and the rest the region.
	for id, want := range map[uint16]string{
		0x0804: langZH, // Chinese (China)
		0x0404: langZH, // Chinese (Taiwan)
		0x0c04: langZH, // Chinese (Hong Kong)
		0x1004: langZH, // Chinese (Singapore)
		0x0409: langEN, // English (United States)
		0x0809: langEN, // English (United Kingdom)
		0x0411: langEN, // Japanese
		0x0407: langEN, // German
		0x0000: langEN,
	} {
		if got := langFromLangID(id); got != want {
			t.Errorf("langFromLangID(%#04x) = %q, want %q", id, got, want)
		}
	}
}

func TestUnixEnvironmentNamesTheLanguageInTheUsualOrder(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", nil, langEN},
		{"LANG alone", map[string]string{"LANG": "zh_CN.UTF-8"}, langZH},
		{"LC_ALL beats LANG", map[string]string{"LC_ALL": "en_US.UTF-8", "LANG": "zh_CN.UTF-8"}, langEN},
		{"LC_MESSAGES beats LANG", map[string]string{"LC_MESSAGES": "zh_CN.UTF-8", "LANG": "en_US.UTF-8"}, langZH},
		{"LC_ALL beats LC_MESSAGES", map[string]string{"LC_ALL": "zh_CN", "LC_MESSAGES": "en_US"}, langZH},
		{"an empty one is not set", map[string]string{"LC_ALL": "", "LANG": "zh_TW"}, langZH},
	}
	for _, c := range cases {
		if got := langFromEnv(env(c.env)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// detectSystemLang itself answers with one of the two on any platform.
	if got := detectSystemLang(); got != langEN && got != langZH {
		t.Errorf("detectSystemLang() = %q", got)
	}
}

func TestAutoFollowsTheSystemAndAChoiceOverridesIt(t *testing.T) {
	asIfTheSystemSpeaks(t, langZH)
	m := NewManager(t.TempDir(), 2)
	if m.LanguageSetting() != langAuto || m.Lang() != langZH {
		t.Fatalf("a new manager is %q speaking %q, want auto speaking zh", m.LanguageSetting(), m.Lang())
	}
	setLanguage(t, m, langEN)
	if m.Lang() != langEN {
		t.Errorf("English was chosen but godm speaks %q", m.Lang())
	}
	setLanguage(t, m, langAuto)
	if m.Lang() != langZH {
		t.Errorf("back to auto, godm speaks %q, want zh", m.Lang())
	}

	// The system's language is looked at when it is asked, not once at start-up.
	systemLang = func() string { return langEN }
	if m.Lang() != langEN {
		t.Errorf("the system changed to English, godm speaks %q", m.Lang())
	}
}

// ---------- the setting ----------

func TestLanguageSettingRoundTrip(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	call := settingsAPI(t, m)

	code, got := call("GET", "", "tok")
	if code != 200 || got["language"] != "auto" || got["language_active"] != "en" {
		t.Fatalf("GET = %d %v, want auto, active in English", code, got)
	}

	code, got = call("POST", `{"language":"zh"}`, "tok")
	if code != 200 || got["language"] != "zh" || got["language_active"] != "zh" {
		t.Fatalf("POST zh = %d %v", code, got)
	}
	if m.Lang() != langZH {
		t.Error("the manager did not take the change")
	}
	if _, got = call("GET", "", "tok"); got["language"] != "zh" {
		t.Errorf("GET after = %v", got["language"])
	}

	// Leaving it out leaves it alone; so does a request that is refused.
	if _, got = call("POST", `{"keep_awake":false}`, "tok"); got["language"] != "zh" {
		t.Errorf("a request without a language changed it to %v", got["language"])
	}
	for _, bad := range []string{`{"language":"fr"}`, `{"language":""}`, `{"language":"zh-CN"}`, `{"language":"ZH"}`} {
		if code, _ = call("POST", bad, "tok"); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
	if m.LanguageSetting() != langZH {
		t.Errorf("a refused request changed the language to %q", m.LanguageSetting())
	}
	// A refusal changes nothing else in the same request either.
	if code, _ = call("POST", `{"sort_by_type":true,"language":"fr"}`, "tok"); code != http.StatusBadRequest || m.SortByType() {
		t.Errorf("a refused request still applied the rest (status %d, sorting %v)", code, m.SortByType())
	}

	code, got = call("POST", `{"language":"auto"}`, "tok")
	if code != 200 || got["language"] != "auto" {
		t.Errorf("POST auto = %d %v", code, got)
	}
}

func TestLanguageSettingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 2)
	m.store = filepath.Join(dir, "tasks.json")
	setLanguage(t, m, langZH)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(dir, 2)
	m2.store = m.store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if m2.LanguageSetting() != langZH {
		t.Errorf("after a restart the language is %q, want zh", m2.LanguageSetting())
	}
}

func TestLanguageSettingFromAnOlderFileOrAMangledOne(t *testing.T) {
	for name, file := range map[string]string{
		"written before there was a language": `{"version":1,"tasks":[],"settings":{"keep_awake":false}}`,
		"written before there were settings":  `{"version":1,"tasks":[]}`,
		"an empty language":                   `{"version":1,"tasks":[],"settings":{"language":""}}`,
		"a language godm does not have":       `{"version":1,"tasks":[],"settings":{"language":"klingon"}}`,
	} {
		dir := t.TempDir()
		store := filepath.Join(dir, "tasks.json")
		if err := os.WriteFile(store, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		m := NewManager(dir, 2)
		m.store = store
		if err := m.load(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := m.LanguageSetting(); got != langAuto {
			t.Errorf("%s: language = %q, want auto", name, got)
		}
		if got := m.settingsView()["language"]; got != langAuto {
			t.Errorf("%s: the settings view says %v, want auto", name, got)
		}
	}
}

// The page learns the language from what it already polls, so another window
// follows a change, and gets it in the page itself so the first paint is right.
func TestThePageIsToldTheLanguage(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	s := &server{mgr: m, token: "tok"}
	page := func() string {
		rec := httptest.NewRecorder()
		s.handleUI(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String()
	}
	if got := page(); !strings.Contains(got, `var LANG_SETTING = "auto";`) || strings.Contains(got, "__LANG__") || strings.Contains(got, "__TOKEN__") {
		t.Errorf("the page does not carry the setting, or a placeholder is left in it")
	}
	setLanguage(t, m, langZH)
	if got := page(); !strings.Contains(got, `var LANG_SETTING = "zh";`) {
		t.Errorf("the page still carries the old setting")
	}

	rec := httptest.NewRecorder()
	s.handleTasks(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if !strings.Contains(rec.Body.String(), `"language":"zh"`) {
		t.Errorf("the task list does not carry the language: %s", rec.Body.String())
	}
}

// ---------- what godm says ----------

func TestMessagesFillInValuesAndFallBackToEnglish(t *testing.T) {
	if got := msg(langEN, "tray.tip.running", "n", 3, "speed", "1.5 MB"); got != "godm — 3 downloading · 1.5 MB/s" {
		t.Errorf("English = %q", got)
	}
	if got := msg(langZH, "tray.tip.running", "n", 3, "speed", "1.5 MB"); !hasHan(got) || !strings.Contains(got, "3") || !strings.Contains(got, "1.5 MB") {
		t.Errorf("Chinese = %q", got)
	}
	// A value that looks like a placeholder is not filled in again.
	if got := msg(langEN, "notify.expiredAt", "name", "{name}.zip"); !strings.HasPrefix(got, "{name}.zip\n") {
		t.Errorf("a value was read as a placeholder: %q", got)
	}
	// Missing: in another language, English; nowhere, the key.
	goStrings["xx"] = map[string]string{}
	defer delete(goStrings, "xx")
	if got := msg("xx", "after.sleep"); got != "Sleep" {
		t.Errorf("a language with no words gives %q, want English", got)
	}
	if got := msg(langEN, "no.such.key"); got != "no.such.key" {
		t.Errorf("an unknown key gives %q", got)
	}
}

func TestTimesAreSaidInTheLanguage(t *testing.T) {
	for secs, want := range map[int64]string{0: "0s", 5: "5s", 59: "59s", 60: "1m 0s", 200: "3m 20s", 3599: "59m 59s", 3600: "1h 0m", 7500: "2h 5m"} {
		if got := span(langEN, secs); got != want {
			t.Errorf("span(en, %d) = %q, want %q", secs, got, want)
		}
		if got := span(langZH, secs); !hasHan(got) {
			t.Errorf("span(zh, %d) = %q has no Chinese in it", secs, got)
		}
	}
}

func TestNotificationsAreInTheChosenLanguage(t *testing.T) {
	m, _, n := managerWithFakePC(t, 2)
	setLanguage(t, m, langZH)

	mt := &managedTask{
		view:   TaskView{ID: "x", Filename: "movie.mkv", Size: 4 << 20, StartedAt: time.Now().Add(-3 * time.Second)},
		outDir: t.TempDir(),
	}
	mt.gen = 1
	m.tasks["x"] = mt
	m.order = append(m.order, "x")

	m.finishRun(mt, 1, StateDone, "", `C:\x\movie.mkv`)
	waitFor(t, 2*time.Second, "the completion notice", func() bool { return len(n.all()) == 1 })
	got := n.all()[0]
	if !strings.HasPrefix(got, msg(langZH, "notify.complete")+" | ") || !hasHan(got) || !strings.Contains(got, "movie.mkv") ||
		!strings.Contains(got, "4.0 MiB") || !strings.Contains(got, msg(langZH, "time.s", "s", 3)) {
		t.Errorf("completion notice = %q, want the Chinese title, the name, the size and 3 seconds in Chinese", got)
	}

	m.finishRun(mt, 1, StateNeedsRefresh, "download link expired: 403", "")
	waitFor(t, 2*time.Second, "the expiry notice", func() bool { return len(n.all()) == 2 })
	if got := n.all()[1]; !strings.HasPrefix(got, msg(langZH, "notify.expired")+" | ") || !hasHan(got) {
		t.Errorf("expiry notice = %q", got)
	}

	// The error text is the daemon's own and stays as it is.
	m.finishRun(mt, 1, StateError, "disk full", "")
	waitFor(t, 2*time.Second, "the failure notice", func() bool { return len(n.all()) == 3 })
	if got := n.all()[2]; !strings.HasPrefix(got, msg(langZH, "notify.failed")+" | ") || !strings.HasSuffix(got, "disk full") {
		t.Errorf("failure notice = %q", got)
	}

	// Back in English the same event is in English: no restart in between.
	setLanguage(t, m, langEN)
	m.finishRun(mt, 1, StateError, "disk full", "")
	waitFor(t, 2*time.Second, "the English failure notice", func() bool { return len(n.all()) == 4 })
	if got := n.all()[3]; !strings.HasPrefix(got, "Download failed | ") {
		t.Errorf("after switching to English the notice is %q", got)
	}
}

func TestShutdownWarningAndReasonAreInTheChosenLanguage(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	setLanguage(t, m, langZH)
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "shutdown")
	end(m, only, StateDone)

	happensOnce(t, pc, "shutdown in 1m0s")
	pc.mu.Lock()
	reason := pc.reason
	pc.mu.Unlock()
	if reason != msg(langZH, "power.shutReason") || !hasHan(reason) {
		t.Errorf("Windows is told %q, want the Chinese reason", reason)
	}
	waitFor(t, 5*time.Second, "the warning", func() bool { return len(n.withTitle(msg(langZH, "power.shutTitle", "n", 60))) > 0 })
	if got := n.withTitle(msg(langZH, "power.shutTitle", "n", 60))[0]; !strings.Contains(got, "shutdown /a") {
		t.Errorf("warning = %q, want it to say how to cancel", got)
	}
}

// ---------- every key is there ----------

// Keys are written "area.name", and a string in the source that is exactly that
// is taken to be one. It is how a key is found wherever it is used: in a call,
// in a table of keys, in an attribute.
var keyShape = `[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+`

// placeholders are the {names} in a message, sorted.
func placeholders(s string) string {
	var out []string
	for _, m := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

var dictLine = regexp.MustCompile(`^\s*("(?:[^"\\]|\\.)*"):\s*("(?:[^"\\]|\\.)*"),?\s*$`)

// parseDict reads the lines of a table written as "key": "text", one to a line,
// and fails on anything else, which keeps the tables simple enough to read here.
func parseDict(t *testing.T, what, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if s := strings.TrimSpace(line); s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		m := dictLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: cannot read this line as \"key\": \"text\",\n%s", what, line)
		}
		k, err1 := strconv.Unquote(m[1])
		v, err2 := strconv.Unquote(m[2])
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: bad string on the line\n%s", what, line)
		}
		if _, dup := out[k]; dup {
			t.Errorf("%s: %q is defined twice", what, k)
		}
		out[k] = v
	}
	return out
}

// sameKeys checks that two languages have the same keys, the same placeholders
// in each, and no empty text.
func sameKeys(t *testing.T, what string, en, zh map[string]string) {
	t.Helper()
	for k, e := range en {
		z, ok := zh[k]
		switch {
		case !ok:
			t.Errorf("%s: %q has no Chinese", what, k)
		case strings.TrimSpace(z) == "":
			t.Errorf("%s: %q is empty in Chinese", what, k)
		case placeholders(e) != placeholders(z):
			t.Errorf("%s: %q has {%s} in English and {%s} in Chinese", what, k, placeholders(e), placeholders(z))
		}
		if strings.TrimSpace(e) == "" {
			t.Errorf("%s: %q is empty in English", what, k)
		}
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("%s: %q is in Chinese but not in English", what, k)
		}
	}
}

func TestEveryKeyTheGoCodeUsesIsInBothLanguages(t *testing.T) {
	en, zh := goStrings[langEN], goStrings[langZH]
	sameKeys(t, "i18n.go", en, zh)

	areas := map[string]bool{}
	for k := range en {
		areas[strings.SplitN(k, ".", 2)[0]] = true
	}
	var names []string
	for a := range areas {
		names = append(names, regexp.QuoteMeta(a))
	}
	sort.Strings(names)
	use := regexp.MustCompile(`"((?:` + strings.Join(names, "|") + `)\.[A-Za-z0-9_.]+)"`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if dictLine.MatchString(line) {
				continue // the tables themselves
			}
			for _, m := range use.FindAllStringSubmatch(line, -1) {
				used[m[1]] = f
			}
		}
	}
	for k, f := range used {
		if _, ok := en[k]; !ok {
			t.Errorf("%s uses %q, which i18n.go does not define", f, k)
		}
	}
	for k := range en {
		if _, ok := used[k]; !ok {
			t.Errorf("%q is in i18n.go but nothing uses it", k)
		}
	}
}

// block is the text between the line that opens a table in the page and the
// line that closes it.
func block(t *testing.T, page, open string) (body, rest string) {
	t.Helper()
	start := strings.Index(page, open+"\n")
	if start < 0 {
		t.Fatalf("the page has no %q", open)
	}
	// The newline that ends the opening line also starts the closing one when
	// the table is empty.
	from := start + len(open)
	end := strings.Index(page[from:], "\n};")
	if end < 0 {
		t.Fatalf("the page does not close %q", open)
	}
	end += from
	if end > from {
		body = page[from+1 : end]
	}
	return body, page[:start] + page[end+3:]
}

func TestEveryKeyThePageUsesIsInBothLanguages(t *testing.T) {
	enBody, rest := block(t, uiHTML, "I18N.en = {")
	zhBody, rest := block(t, rest, "I18N.zh = {")
	en, zh := parseDict(t, "I18N.en", enBody), parseDict(t, "I18N.zh", zhBody)
	if len(en) < 100 {
		t.Fatalf("only %d English strings found; the table was probably not read properly", len(en))
	}
	sameKeys(t, "the page", en, zh)

	// A key with a count has a .one and an .other, asked for as the key between.
	plural := map[string]bool{}
	for _, m := range regexp.MustCompile(`\btrn\("(`+keyShape+`)"`).FindAllStringSubmatch(rest, -1) {
		plural[m[1]] = true
	}
	for base := range plural {
		for _, form := range []string{".one", ".other"} {
			if _, ok := en[base+form]; !ok {
				t.Errorf("trn(%q) needs %q", base, base+form)
			}
		}
	}

	// A key looked up through a table (the words for each state, say) is found
	// where the table names it, since any string of this shape is taken for one.
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(`+keyShape+`)"`).FindAllStringSubmatch(rest, -1) {
		used[m[1]] = true
		k := m[1]
		if _, ok := en[k]; !ok && !plural[k] {
			t.Errorf("the page uses %q, which the table does not define", k)
		}
	}
	for k := range en {
		base := strings.TrimSuffix(strings.TrimSuffix(k, ".one"), ".other")
		if !used[k] && !(plural[base] && base != k) {
			t.Errorf("%q is in the table but the page never uses it", k)
		}
	}

	// What goes in as text is not read as markup.
	for _, m := range regexp.MustCompile(`data-i18n(?:-title|-ph)?="(`+keyShape+`)"`).FindAllStringSubmatch(rest, -1) {
		for lang, dict := range map[string]map[string]string{"en": en, "zh": zh} {
			if strings.ContainsAny(dict[m[1]], "<>") {
				t.Errorf("%s %q is set as plain text but has markup in it: %q", lang, m[1], dict[m[1]])
			}
		}
	}
}

// Both a placeholder and a markup tag have to survive translation, or the page
// shows a stray {n} or breaks its layout.
func TestChineseKeepsTheMarkupOfTheEnglish(t *testing.T) {
	enBody, rest := block(t, uiHTML, "I18N.en = {")
	zhBody, _ := block(t, rest, "I18N.zh = {")
	en, zh := parseDict(t, "I18N.en", enBody), parseDict(t, "I18N.zh", zhBody)
	tags := regexp.MustCompile(`</?[a-z]+[^>]*>`)
	for k, e := range en {
		z := zh[k]
		if a, b := strings.Join(tags.FindAllString(e, -1), ""), strings.Join(tags.FindAllString(z, -1), ""); a != b {
			t.Errorf("%q: tags %q in English, %q in Chinese", k, a, b)
		}
		if !hasHan(z) && !isNameOnly(e) {
			t.Errorf("%q has no Chinese in it: %q", k, z)
		}
	}
}

// isNameOnly is a string with nothing in it to translate.
func isNameOnly(s string) bool {
	s = regexp.MustCompile(`<[^>]*>|\{\w+\}`).ReplaceAllString(s, "")
	return strings.Trim(s, "#%/·—… 0123456789") == "" || strings.EqualFold(s, "godm")
}

// The Settings dialog names each seeding choice in its own words, keyed by the
// value the daemon uses, so a choice the daemon gains needs words there too.
func TestEverySeedingChoiceHasWordsOnThePage(t *testing.T) {
	m := regexp.MustCompile(`var SEED_LABEL = \{([^}]*)\};`).FindStringSubmatch(uiHTML)
	if m == nil {
		t.Fatal("the page has no SEED_LABEL")
	}
	onPage := map[string]string{}
	for _, kv := range regexp.MustCompile(`(\w+): "([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		onPage[kv[1]] = kv[2]
	}
	for _, c := range seedChoices {
		if onPage[c["value"]] == "" {
			t.Errorf("the page has no words for the seeding choice %q", c["value"])
		}
	}
	if len(onPage) != len(seedChoices) {
		t.Errorf("the page names %d seeding choices, the daemon offers %d", len(onPage), len(seedChoices))
	}
}

// The page is one long script inside a Go string, which nothing else parses.
func TestThePageScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	m := regexp.MustCompile(`(?s)<script>\n(.*?)\n</script>`).FindStringSubmatch(uiHTML)
	if m == nil {
		t.Fatal("no script in the page")
	}
	file := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(file, []byte(m[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
		t.Fatalf("the page script does not parse:\n%s", out)
	}
}

func TestTheGoTablesKeepTheirPlaceholdersAndTranslateEverything(t *testing.T) {
	for k, e := range goStrings[langEN] {
		z := goStrings[langZH][k]
		if !hasHan(z) && !isNameOnly(e) {
			t.Errorf("%q has no Chinese in it: %q", k, z)
		}
	}
}
