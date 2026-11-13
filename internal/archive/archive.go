package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
	"gopkg.in/yaml.v3"
)

// HardcodedIgnorePatterns are always excluded for security.
var HardcodedIgnorePatterns = []string{
	// Version control
	".git",
	".git/**",
	".svn",
	".hg",

	// Local CLI state (template key of `taufinity preview`), never uploaded
	".taufinity",
	".taufinity/**",

	// Environment and secrets
	".env",
	".env*",
	".env.*",
	"*.env",

	// Credentials and keys
	"credentials.json",
	"credentials.yaml",
	"secrets.yaml",
	"secrets.json",
	"*.key",
	"*.pem",
	"*.p12",
	"*.pfx",

	// More secrets, excluded from every upload (preview, deploy, ...): the
	// CLI's own credentials-<host>.json, package manager and netrc logins,
	// SSH keys, cloud CLI folders, Terraform variables and state, service
	// account keys.
	"credentials*.json",
	".npmrc",
	".netrc",
	".pypirc",
	"id_rsa*",
	"id_ed25519*",
	"id_ecdsa*",
	".aws",
	".aws/**",
	".ssh",
	".ssh/**",
	".gcloud",
	".gcloud/**",
	"*.tfvars",
	"*.tfstate",
	"*.tfstate.*",
	"*-sa.json",
	"service-account*.json",

	// IDE and OS
	".idea",
	".vscode",
	".DS_Store",
	"Thumbs.db",

	// Node
	"node_modules",
}

// Create creates a tar.gz archive of the directory, respecting ignore patterns.
func Create(srcDir, destPath string) error {
	// Collect all ignore patterns
	patterns := CollectIgnorePatterns(srcDir)
	matcher := ignore.CompileIgnoreLines(patterns...)

	// Create output file
	outFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create archive file: %w", err)
	}
	defer outFile.Close()

	// Create gzip writer
	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	// Create tar writer
	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	// Walk the directory
	srcDir = filepath.Clean(srcDir)
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		// Skip root directory
		if relPath == "." {
			return nil
		}

		// Check if ignored
		if matcher.MatchesPath(relPath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip directories (they're implicit in tar)
		if info.IsDir() {
			return nil
		}

		// Only regular files: a symlink would pull its target (possibly outside
		// the folder) into the archive. Same rule as CreateZip.
		if !info.Mode().IsRegular() {
			return nil
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("create header for %s: %w", relPath, err)
		}
		header.Name = relPath

		// Write header
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", relPath, err)
		}

		// Write file content
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		defer file.Close()

		if _, err := io.Copy(tarWriter, file); err != nil {
			return fmt.Errorf("copy %s: %w", relPath, err)
		}

		return nil
	})
}

// CollectIgnorePatterns gathers ignore patterns from all sources.
func CollectIgnorePatterns(dir string) []string {
	var patterns []string

	// 1. Hardcoded security patterns
	patterns = append(patterns, HardcodedIgnorePatterns...)

	// 2. .gitignore, from the git root down to the folder itself (a template
	// folder inside a repo is covered by the repo's own .gitignore files)
	patterns = append(patterns, parentGitignores(dir)...)
	patterns = append(patterns, loadIgnoreFile(filepath.Join(dir, ".gitignore"))...)

	// 3. .dockerignore
	patterns = append(patterns, loadIgnoreFile(filepath.Join(dir, ".dockerignore"))...)

	// 4. .taufinityignore
	patterns = append(patterns, loadIgnoreFile(filepath.Join(dir, ".taufinityignore"))...)

	// 5. taufinity.yaml ignore section
	patterns = append(patterns, loadTaufinityIgnores(dir)...)

	return patterns
}

// loadIgnoreFile reads patterns from a gitignore-style file.
func loadIgnoreFile(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// loadTaufinityIgnores reads ignore patterns from taufinity.yaml.
func loadTaufinityIgnores(dir string) []string {
	path := filepath.Join(dir, "taufinity.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cfg struct {
		Ignore []string `yaml:"ignore"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	return cfg.Ignore
}

// parentGitignores returns the patterns of every .gitignore above dir, up to
// and including the git root (the nearest ancestor holding .git). Outside a git
// repo it returns nothing. Outermost first, so nearer files override. A pattern
// anchored to its own folder is rewritten relative to dir, or dropped when it
// points elsewhere.
func parentGitignores(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		return nil // dir is itself the git root
	}
	var chain []string // dir's ancestors, nearest first, up to the git root
	found := false
	for cur := filepath.Dir(abs); ; cur = filepath.Dir(cur) {
		chain = append(chain, cur)
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			found = true
			break
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	if !found {
		return nil
	}
	var out []string
	for i := len(chain) - 1; i >= 0; i-- {
		rel, err := filepath.Rel(chain[i], abs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		for _, p := range loadIgnoreFile(filepath.Join(chain[i], ".gitignore")) {
			if q, ok := rebasePattern(p, rel); ok {
				out = append(out, q)
			}
		}
	}
	return out
}

// rebasePattern makes a pattern of an ancestor's .gitignore relative to dir
// (rel is dir's path below that ancestor). Unanchored patterns apply anywhere.
func rebasePattern(p, rel string) (string, bool) {
	neg := ""
	if strings.HasPrefix(p, "!") {
		neg, p = "!", p[1:]
	}
	body := strings.TrimSuffix(p, "/")
	if !strings.HasPrefix(body, "/") && !strings.Contains(body, "/") {
		return neg + p, true
	}
	body = strings.TrimPrefix(p, "/")
	if body == rel || strings.HasPrefix(body, rel+"/") {
		rest := strings.TrimPrefix(strings.TrimPrefix(body, rel), "/")
		if rest == "" {
			return "", false
		}
		return neg + rest, true
	}
	if strings.HasPrefix(body, "**/") {
		return neg + body, true
	}
	return "", false
}
