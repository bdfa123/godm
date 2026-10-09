//go:build windows

package main

var procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")

// detectSystemLang is the language Windows shows its own menus in. That is the
// display language the person chose, which can differ from the region they live
// in, so it is asked for rather than read from the locale.
func detectSystemLang() string {
	id, _, _ := procGetUserDefaultUILanguage.Call()
	return langFromLangID(uint16(id))
}
