package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type UserPrefs struct {
	Game              string `json:"game"`
	Pages             int    `json:"pages"`
	PageSize          int    `json:"page_size"`
	IncludeMarketInfo bool   `json:"include_market_info"`
	VerifyMarket      bool   `json:"verify_market"`
	TelegramEnabled   bool   `json:"telegram_enabled"`
	TelegramChat      string `json:"telegram_chat"`
	RememberAPI       bool   `json:"remember_api"`
	RememberTelegram  bool   `json:"remember_telegram"`
	ExtraGame         string `json:"extra_game"`
	ExtraMarket       string `json:"extra_market"`
	ExtraMethod       string `json:"extra_method"`
	ExtraItem         string `json:"extra_item"`
	ExtraCompareJSON  string `json:"extra_compare_json"`
	ExtraDays         int    `json:"extra_days"`
	ExcludeSticker    bool   `json:"exclude_sticker"`
	ExcludeCase       bool   `json:"exclude_case"`
	ExcludeSouvenir   bool   `json:"exclude_souvenir"`
}

func defaultPrefs() UserPrefs {
	return UserPrefs{Game: "both", Pages: 1, PageSize: 100, ExtraGame: "CsGo", ExtraMarket: "Tm", ExtraMethod: "advanced", ExtraDays: 30, RememberAPI: true}
}
func (p UserPrefs) valid() error {
	if _, err := (PulseOptions{Pages: p.Pages, PageSize: p.PageSize, Game: p.Game}).normalized(); err != nil {
		return err
	}
	if p.ExtraGame != "CsGo" && p.ExtraGame != "Dota2" {
		return errors.New("неверная игра дополнительного API")
	}
	if p.ExtraMarket != "Tm" && p.ExtraMarket != "Buff" {
		return errors.New("неверная площадка дополнительного API")
	}
	if p.ExtraDays < 1 || p.ExtraDays > 365 {
		return errors.New("период сравнения должен быть 1–365 дней")
	}
	if len(p.ExtraItem) > 300 || len(p.TelegramChat) > 256 || len(p.ExtraCompareJSON) > 12000 {
		return errors.New("слишком длинное поле")
	}
	return nil
}
func prefsFile() string             { return filepath.Join(filepath.Dir(settingsFile()), "preferences.json") }
func secretFile(name string) string { return filepath.Join(filepath.Dir(settingsFile()), name+".bin") }
func loadPrefs() UserPrefs {
	p := defaultPrefs()
	if b, err := os.ReadFile(prefsFile()); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	if p.ExtraGame == "" {
		p.ExtraGame = "CsGo"
	}
	if p.ExtraMarket == "" {
		p.ExtraMarket = "Tm"
	}
	if p.ExtraDays == 0 {
		p.ExtraDays = 30
	}
	if p.PageSize == 0 {
		p.PageSize = 100
	}
	if p.Pages == 0 {
		p.Pages = 1
	}
	if p.Game == "" {
		p.Game = "both"
	}
	if p.valid() != nil {
		return defaultPrefs()
	}
	return p
}
func atomicFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Windows cannot rename over an existing destination, remove only after temp is fully written.
	if err = os.Rename(f.Name(), path); err == nil {
		return nil
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(f.Name(), path)
}
func savePrefs(p UserPrefs) error {
	if err := p.valid(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(prefsFile(), b, 0600)
}
func saveSecret(name, value string, remember bool) error {
	path := secretFile(name)
	if !remember || strings.TrimSpace(value) == "" {
		_ = os.Remove(path)
		return nil
	}
	data, err := protectSecret([]byte(value))
	if err != nil {
		return err
	}
	return atomicFile(path, data, 0600)
}
func loadSecret(name string) (string, error) {
	b, e := os.ReadFile(secretFile(name))
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	plain, e := unprotectSecret(b)
	if e != nil {
		return "", e
	}
	return string(plain), nil
}
