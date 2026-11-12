package commands

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	bannerStateDir  = ".taufinity"
	bannerKeyFile   = "preview.json"
	bannerKeyLength = 22
	bannerKeyChars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
)

var bannerKeyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type bannerKeyDoc struct {
	TemplateKey string `json:"template_key"`
}

func newBannerKey() (string, error) {
	b := make([]byte, bannerKeyLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = bannerKeyChars[int(b[i])%len(bannerKeyChars)] // 64 chars: no modulo bias
	}
	return string(b), nil
}

// templateKeyFor returns the template key of a folder, creating
// <dir>/.taufinity/preview.json on first use. The key is random and holds no
// secret; it is never part of the template ZIP.
func templateKeyFor(dir string) (string, error) {
	path := filepath.Join(dir, bannerStateDir, bannerKeyFile)
	if raw, err := os.ReadFile(path); err == nil {
		var doc bannerKeyDoc
		if json.Unmarshal(raw, &doc) == nil && bannerKeyRE.MatchString(doc.TemplateKey) {
			return doc.TemplateKey, nil
		}
		return "", fmt.Errorf("%s is onleesbaar; verwijder het bestand om een nieuwe sleutel te maken", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	key, err := newBannerKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	raw, _ := json.MarshalIndent(bannerKeyDoc{TemplateKey: key}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	return key, nil
}
