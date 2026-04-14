package archive

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	ignore "github.com/sabhiram/go-gitignore"
)

// CreateZip writes a zip archive of srcDir into w, respecting the same
// ignore rules as Create (hardcoded security excludes + .gitignore +
// .dockerignore + .taufinityignore + taufinity.yaml).
//
// Returns the number of files written. An empty source directory (zero
// files after filtering) is reported as an error so callers don't silently
// upload empty archives.
func CreateZip(srcDir string, w io.Writer) (int, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", srcDir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", srcDir)
	}

	patterns := CollectIgnorePatterns(srcDir)
	matcher := ignore.CompileIgnoreLines(patterns...)

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	srcDir = filepath.Clean(srcDir)
	fileCount := 0

	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		// Zip entries always use forward slashes regardless of OS.
		zipPath := filepath.ToSlash(relPath)

		if matcher.MatchesPath(relPath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("header for %s: %w", relPath, err)
		}
		header.Name = zipPath
		header.Method = zip.Deflate

		entryWriter, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("create entry %s: %w", relPath, err)
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		if _, copyErr := io.Copy(entryWriter, file); copyErr != nil {
			file.Close()
			return fmt.Errorf("copy %s: %w", relPath, copyErr)
		}
		file.Close()
		fileCount++
		return nil
	})
	if err != nil {
		return fileCount, err
	}

	if fileCount == 0 {
		return 0, fmt.Errorf("no files to zip in %s (all filtered or empty)", srcDir)
	}

	return fileCount, nil
}
