package main

import (
	"fmt"
	"strings"
)

// The words godm itself says outside the manager page: the tray menu and its
// tooltip, the notifications, and what Windows shows while it counts down to a
// shutdown. The manager page has a table of its own in webui.go, and the
// browser extension uses Chrome's _locales; each of the three follows the
// language its own surroundings choose, and this one follows the Language
// setting.
//
// English is the language the program is written in and the fallback for a key
// another language lacks. To add a string: one line in goStrings[langEN], one
// in goStrings[langZH], then m.tr("the.key") where it is shown. {name} marks a
// value the caller fills in. i18n_test.go fails if a key is used and missing,
// or the two tables drift apart.

const (
	langAuto = "auto" // follow the operating system
	langEN   = "en"
	langZH   = "zh"
)

// systemLang is the language the operating system is set to, as langEN or
// langZH. It is a variable so that tests do not depend on the machine they run
// on; detectSystemLang is in lang_windows.go and lang_other.go.
var systemLang = detectSystemLang

// validLang says whether s can be saved as the Language setting.
func validLang(s string) bool {
	return s == langAuto || s == langEN || s == langZH
}

// cleanLang makes a value read from disk usable: anything that is not a
// language godm has is the same as never having chosen one.
func cleanLang(s string) string {
	if validLang(s) {
		return s
	}
	return langAuto
}

// resolveLang is the language to speak for a Language setting.
func resolveLang(setting string) string {
	if setting == langEN || setting == langZH {
		return setting
	}
	return systemLang()
}

// langFromTag reads a locale name the way a browser or a shell writes one,
// "zh-CN", "zh_Hans_CN.UTF-8", "en_US". Every Chinese locale gets the one
// Chinese translation there is.
func langFromTag(tag string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(tag)), "zh") {
		return langZH
	}
	return langEN
}

// langFromLangID reads a Windows language identifier. The low ten bits are the
// primary language and the rest the region, so 0x0804 (Chinese, China) and
// 0x0404 (Chinese, Taiwan) are both 0x04.
func langFromLangID(id uint16) string {
	const primaryChinese = 0x04
	if id&0x3ff == primaryChinese {
		return langZH
	}
	return langEN
}

// langFromEnv is the language of a Unix environment, which names it in the
// first of these that is set.
func langFromEnv(getenv func(string) string) string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(name); v != "" {
			return langFromTag(v)
		}
	}
	return langEN
}

// msg is the text for key in lang. kv holds pairs of a placeholder name and the
// value for it. A key that is missing everywhere comes back as itself, which is
// ugly enough to be noticed.
func msg(lang, key string, kv ...any) string {
	s, ok := goStrings[lang][key]
	if !ok {
		s, ok = goStrings[langEN][key]
	}
	if !ok {
		return key
	}
	if len(kv) < 2 {
		return s
	}
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, "{"+fmt.Sprint(kv[i])+"}", fmt.Sprint(kv[i+1]))
	}
	// One pass, so that a file name which happens to contain "{n}" stays as it is.
	return strings.NewReplacer(pairs...).Replace(s)
}

// Lang is the language godm is speaking now.
func (m *Manager) Lang() string {
	return resolveLang(m.LanguageSetting())
}

// LanguageSetting is what the Language setting holds: auto, en or zh.
func (m *Manager) LanguageSetting() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cleanLang(m.settings.Language)
}

// tr is msg in the language godm is speaking.
func (m *Manager) tr(key string, kv ...any) string {
	return msg(m.Lang(), key, kv...)
}

// span is a length of time as a person says it: "5s", "3m 20s", "2h 5m".
func span(lang string, secs int64) string {
	switch {
	case secs < 60:
		return msg(lang, "time.s", "s", secs)
	case secs < 3600:
		return msg(lang, "time.ms", "m", secs/60, "s", secs%60)
	}
	return msg(lang, "time.hm", "h", secs/3600, "m", secs%3600/60)
}

var goStrings = map[string]map[string]string{
	langEN: {
		"tray.open":        "Open godm",
		"tray.pauseAll":    "Pause all",
		"tray.resumeAll":   "Resume all",
		"tray.folder":      "Open downloads folder",
		"tray.notify":      "Notify when downloads finish",
		"tray.after":       "When all downloads finish",
		"tray.quit":        "Quit godm",
		"tray.browse":      "Save downloads to",
		"tray.tip.idle":    "godm — idle",
		"tray.tip.running": "godm — {n} downloading · {speed}/s",
		"tray.tip.queued":  "godm — {n} waiting",
		"tray.tip.more":    " · {n} waiting",
		"tray.tip.sleep":   " · sleeps when done",
		"tray.tip.shut":    " · shuts down when done",
		"after.nothing":    "Do nothing",
		"after.sleep":      "Sleep",
		"after.shutdown":   "Shut down",
		"notify.complete":  "Download complete",
		"notify.sizeIn":    "{size} in {time}",
		"notify.expired":   "Download link expired",
		"notify.expiredAt": "{name}\nOpen godm to fetch a fresh link and continue.",
		"notify.failed":    "Download failed",
		"power.sleepFail":  "Could not put the computer to sleep",
		"power.shutFail":   "Could not shut the computer down",
		"power.shutTitle":  "Shutting down in {n} seconds",
		"power.shutText":   "All downloads finished. To cancel, press Win+R, type shutdown /a and press Enter.",
		"power.shutReason": "godm: all downloads finished",
		"time.s":           "{s}s",
		"time.ms":          "{m}m {s}s",
		"time.hm":          "{h}h {m}m",
	},
	langZH: {
		"tray.open":        "打开 godm",
		"tray.pauseAll":    "全部暂停",
		"tray.resumeAll":   "全部继续",
		"tray.folder":      "打开下载文件夹",
		"tray.notify":      "下载完成时通知",
		"tray.after":       "所有下载完成后",
		"tray.quit":        "退出 godm",
		"tray.browse":      "选择保存下载文件的文件夹",
		"tray.tip.idle":    "godm — 空闲",
		"tray.tip.running": "godm — {n} 个任务下载中 · {speed}/s",
		"tray.tip.queued":  "godm — {n} 个任务等待中",
		"tray.tip.more":    " · {n} 个任务等待中",
		"tray.tip.sleep":   " · 完成后进入睡眠",
		"tray.tip.shut":    " · 完成后关机",
		"after.nothing":    "不执行任何操作",
		"after.sleep":      "睡眠",
		"after.shutdown":   "关机",
		"notify.complete":  "下载完成",
		"notify.sizeIn":    "{size}，用时 {time}",
		"notify.expired":   "下载链接已过期",
		"notify.expiredAt": "{name}\n打开 godm 获取新链接并继续。",
		"notify.failed":    "下载失败",
		"power.sleepFail":  "无法让电脑进入睡眠",
		"power.shutFail":   "无法让电脑关机",
		"power.shutTitle":  "将在 {n} 秒后关机",
		"power.shutText":   "所有下载已完成。若要取消，请按 Win+R，输入 shutdown /a 并按回车。",
		"power.shutReason": "godm：所有下载已完成",
		"time.s":           "{s}秒",
		"time.ms":          "{m}分{s}秒",
		"time.hm":          "{h}小时{m}分",
	},
}
