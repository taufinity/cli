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

	// 2. .gitignore
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
