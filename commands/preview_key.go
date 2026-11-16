package commands

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

var bannerKeyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`) // the server refuses longer keys

type bannerKeyDoc struct {
	TemplateKey string `json:"template_key"`
	// Folder says which folder the key belongs to, as a hash of its real path
	// (no path or user name is stored, since the file may be committed). A
	// preview session is paid per template folder: a copied folder brings this
	// file along, sees a different hash and gets a key, and a session, of its
	// own. Moving a folder does the same. Decided by the owner, 13 Nov 2026.
	Folder string `json:"folder,omitempty"`
}

// bannerFolderID is the hash of a folder's real path, symlinks resolved.
func bannerFolderID(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	sum := sha256.Sum256([]byte("taufinity-preview-folder\x00" + abs))
	return hex.EncodeToString(sum[:16]), nil
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
	folder, err := bannerFolderID(dir)
	if err != nil {
		return "", err
	}
	if err := ignoreBannerStateDir(dir); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 5; attempt++ {
		if raw, err := os.ReadFile(path); err == nil {
			var doc bannerKeyDoc
			if json.Unmarshal(raw, &doc) != nil || !bannerKeyRE.MatchString(doc.TemplateKey) {
				return "", fmt.Errorf("%s is onleesbaar; verwijder het bestand om een nieuwe sleutel te maken", path)
			}
			switch doc.Folder {
			case folder:
				return doc.TemplateKey, nil
			case "":
				// written before keys were bound to a folder: this folder keeps it
				if err := replaceBannerKey(path, bannerKeyDoc{TemplateKey: doc.TemplateKey, Folder: folder}); err != nil {
					return "", err
				}
			default:
				// a copy (or a moved folder): its own key, its own session
				key, err := newBannerKey()
				if err != nil {
					return "", err
				}
				if err := replaceBannerKey(path, bannerKeyDoc{TemplateKey: key, Folder: folder}); err != nil {
					return "", err
				}
			}
			continue // read back what is on disk now: another CLI may have written first
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
		created, err := publishBannerKey(path, bannerKeyDoc{TemplateKey: key, Folder: folder})
		if err != nil {
			return "", err
		}
		if created {
			return key, nil
		}
		// another CLI was first: read its key on the next round
	}
	return "", fmt.Errorf("%s kon niet worden aangemaakt: een ander proces schrijft er ook naar", path)
}

// ignoreBannerStateDir keeps .taufinity/ out of git with a .gitignore of its
// own. The key is bound to a folder on one machine, so a committed key file
// would be rewritten by every other checkout and teammate.
func ignoreBannerStateDir(dir string) error {
	state := filepath.Join(dir, bannerStateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(state, ".gitignore"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, werr := f.WriteString("*\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// publishBannerKey writes the key file atomically and without ever replacing
// an existing one: the content goes to a temp file (created with O_EXCL) in the
// same folder, which is then hard-linked to its final name. link(2) fails when
// the name exists, so of two CLIs starting at once exactly one wins, and a
// reader never sees a half-written file. created is false when another process
// was first. Where hard links are not supported it falls back to rename.
func publishBannerKey(path string, doc bannerKeyDoc) (created bool, err error) {
	tmp, err := writeBannerKeyTemp(path, doc)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		// no hard links on this filesystem: rename (still atomic, last writer wins)
		if _, statErr := os.Stat(path); statErr == nil {
			return false, nil
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			return false, rerr
		}
	}
	return true, nil
}

// replaceBannerKey overwrites the key file atomically (temp file, then rename),
// so a reader sees the old file or the new one, never half of either.
func replaceBannerKey(path string, doc bannerKeyDoc) error {
	tmp, err := writeBannerKeyTemp(path, doc)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeBannerKeyTemp writes doc to a new temp file (O_EXCL) next to path.
func writeBannerKeyTemp(path string, doc bannerKeyDoc) (string, error) {
	raw, _ := json.MarshalIndent(doc, "", "  ")
	raw = append(raw, '\n')
	var tmp string
	for i := 0; ; i++ {
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			return "", err
		}
		tmp = fmt.Sprintf("%s.tmp-%x", path, suffix)
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) && i < 5 {
				continue
			}
			return "", err
		}
		_, werr := f.Write(raw)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(tmp)
			return "", werr
		}
		break
	}
	return tmp, nil
}
