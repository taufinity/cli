package archive

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// ZipOptions bounds and instruments CreateZipWithOptions. The zero value means
// no limits, which is what CreateZip uses.
type ZipOptions struct {
	// MaxFiles refuses a folder holding more files than this (0 = no limit).
	// The count is made up front, before any file is read.
	MaxFiles int
	// MaxBytes stops the zipper as soon as the compressed output passes this
	// many bytes, without reading the rest of the folder (0 = no limit).
	MaxBytes int64
	// Warn receives the one Dutch warning that names the skipped symlinks
	// (nil = stderr).
	Warn func(msg string)
}

// ErrTooManyFiles and ErrTooLarge are the Dutch limit errors.
var (
	ErrTooManyFiles = errors.New("de map bevat te veel bestanden")
	ErrTooLarge     = errors.New("de map is te groot na inpakken")
)

// CreateZip writes a zip archive of srcDir into w, respecting the same
// ignore rules as Create (hardcoded security excludes + .gitignore +
// .dockerignore + .taufinityignore + taufinity.yaml).
//
// Returns the number of files written. An empty source directory (zero
// files after filtering) is reported as an error so callers don't silently
// upload empty archives.
//
// Safety (shared by every caller of this package): only regular files inside
// srcDir are archived. Symlinks, to files and to folders, are skipped with one
// warning, so a link such as logo.png -> ../outside/creds.json can never pull
// a file from outside the folder into an upload.
func CreateZip(srcDir string, w io.Writer) (int, error) {
	return CreateZipWithOptions(context.Background(), srcDir, w, ZipOptions{})
}

// limitWriter counts what is written and fails once the limit is passed.
type limitWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	l.n += int64(n)
	if err == nil && l.limit > 0 && l.n > l.limit {
		return n, ErrTooLarge
	}
	return n, err
}

// walkEligible walks srcDir and calls fn for every regular file that the ignore
// rules let through. Symlinks and other non-regular entries are skipped; their
// relative paths are returned (symlinks only).
func walkEligible(ctx context.Context, srcDir string, matcher *ignore.GitIgnore, fn func(path, rel string, info os.FileInfo) error) ([]string, error) {
	var links []string
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		// a folder rule such as "build/" only matches with a trailing slash
		if matcher.MatchesPath(relPath) || (info.IsDir() && matcher.MatchesPath(filepath.ToSlash(relPath)+"/")) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links = append(links, filepath.ToSlash(relPath))
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // sockets, devices, pipes
		}
		return fn(path, relPath, info)
	})
	return links, err
}

// CreateZipWithOptions is CreateZip with a context, limits and a warning hook.
func CreateZipWithOptions(ctx context.Context, srcDir string, w io.Writer, opts ZipOptions) (int, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", srcDir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", srcDir)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	srcDir = filepath.Clean(srcDir)
	matcher := ignore.CompileIgnoreLines(CollectIgnorePatterns(srcDir)...)

	// Up front: refuse a folder with too many files before reading any.
	var links []string
	if opts.MaxFiles > 0 {
		count := 0
		links, err = walkEligible(ctx, srcDir, matcher, func(_, _ string, _ os.FileInfo) error {
			count++
			if count > opts.MaxFiles {
				return fmt.Errorf("%w: meer dan %d bestanden; pak de sjabloon in een eigen map", ErrTooManyFiles, opts.MaxFiles)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}

	var out io.Writer = w
	if opts.MaxBytes > 0 {
		out = &limitWriter{w: w, limit: opts.MaxBytes}
	}
	zipWriter := zip.NewWriter(out)
	fileCount := 0

	links, err = walkEligible(ctx, srcDir, matcher, func(path, relPath string, info os.FileInfo) error {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("header for %s: %w", relPath, err)
		}
		header.Name = filepath.ToSlash(relPath) // zip entries use forward slashes
		header.Method = zip.Deflate

		entryWriter, err := zipWriter.CreateHeader(header)
		if err != nil {
			return wrapZipErr(opts, "create entry "+relPath, err)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		_, copyErr := io.Copy(entryWriter, file)
		file.Close()
		if copyErr != nil {
			return wrapZipErr(opts, "copy "+relPath, copyErr)
		}
		fileCount++
		return nil
	})
	if err != nil {
		return fileCount, err
	}
	if err := zipWriter.Close(); err != nil {
		return fileCount, wrapZipErr(opts, "close zip", err)
	}

	if len(links) > 0 {
		msg := "Waarschuwing: symbolische links worden niet meegenomen: " + strings.Join(links, ", ")
		if opts.Warn != nil {
			opts.Warn(msg)
		} else {
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	if fileCount == 0 {
		return 0, fmt.Errorf("no files to zip in %s (all filtered or empty)", srcDir)
	}
	return fileCount, nil
}

func wrapZipErr(opts ZipOptions, what string, err error) error {
	if errors.Is(err, ErrTooLarge) {
		return fmt.Errorf("%w: meer dan %d MB; het maximum is %d MB", ErrTooLarge, opts.MaxBytes>>20, opts.MaxBytes>>20)
	}
	return fmt.Errorf("%s: %w", what, err)
}
