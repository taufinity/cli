package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Options selects what to collect.
type Options struct {
	Base   string // explicit base ref; empty = resolve (see ResolveBase)
	Staged bool   // diff the index instead of the working tree
	Whole  bool   // review current content instead of a change
	// Head reviews the commits up to this revision instead of the working
	// tree: uncommitted and untracked files are left out. The pre-push hook
	// passes the local sha of each pushed ref.
	Head  string
	Paths []string
}

// Skipped is a file that was left out, and why.
type Skipped struct{ Path, Reason string }

// Payload is everything the review request needs. Diff and Files are already
// filtered and masked.
type Payload struct {
	Repo, BaseRef, HeadSHA, Mode, Title, Diff string
	Files                                     map[string]string
	Skipped                                   []Skipped
	Masked                                    int
}

// tally keeps a running size so a huge selection is refused as soon as it is
// over the limit, not after everything has been read into memory.
type tally struct{ files, diff int }

func (t *tally) check() error {
	if t.diff > MaxDiffBytes {
		return fmt.Errorf("the diff is %d bytes or more, over the limit of %d bytes (1.5 MiB); narrow it with paths, for example: taufinity review src/api/", t.diff, MaxDiffBytes)
	}
	if t.files > MaxFilesBytes {
		return fmt.Errorf("the files to send are %d bytes or more, over the limit of %d bytes in total; narrow it with paths, for example: taufinity review src/api/", t.files, MaxFilesBytes)
	}
	return nil
}

// pathChunk bounds how many pathspecs go on one git command line.
const pathChunk = 100

// Collect reads the change set from the git repository at dir. Paths are git
// pathspecs relative to dir; the keys of Files are repository-root relative.
func Collect(ctx context.Context, dir string, o Options) (Payload, error) {
	topOut, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Payload{}, fmt.Errorf("not a git repository: %w", err)
	}
	top := strings.TrimSpace(string(topOut))

	p := Payload{Files: map[string]string{}, Mode: "diff", Repo: RepoName(ctx, dir)}
	if o.Whole {
		p.Mode = "whole"
		if len(o.Paths) == 0 {
			return Payload{}, errors.New("--whole needs at least one path, for example: taufinity review --whole src/")
		}
	}
	if o.Head != "" {
		switch {
		case o.Whole || o.Staged:
			return Payload{}, errors.New("--head reviews commits; it cannot be combined with --whole or --staged")
		case strings.HasPrefix(o.Head, "-"):
			// Like --base, the value could reach git as an option.
			return Payload{}, fmt.Errorf("head %q is not a revision", o.Head)
		}
		sha := revParse(ctx, dir, o.Head)
		if sha == "" {
			return Payload{}, fmt.Errorf("head %q does not resolve to a commit", o.Head)
		}
		o.Head, p.HeadSHA = sha, sha
	} else if out, herr := git(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); herr == nil {
		p.HeadSHA = strings.TrimSpace(string(out))
	}
	if out, berr := git(ctx, dir, "branch", "--show-current"); berr == nil {
		p.Title = strings.TrimSpace(string(out))
	}
	if p.Title == "" {
		p.Title = "(detached HEAD)"
	}

	var diff strings.Builder
	if o.Whole {
		p.BaseRef = o.Base
		if ref, _, rerr := ResolveBase(ctx, dir, o.Base); rerr == nil {
			p.BaseRef = ref
		}
		if err := collectWhole(ctx, dir, top, o, &p, &diff); err != nil {
			return Payload{}, err
		}
	} else {
		head := "HEAD"
		if o.Head != "" {
			head = o.Head
		}
		ref, mergeBase, rerr := resolveBaseFor(ctx, dir, o.Base, head)
		if rerr != nil {
			return Payload{}, rerr
		}
		p.BaseRef = ref
		if err := collectDiff(ctx, dir, top, mergeBase, o, &p, &diff); err != nil {
			return Payload{}, err
		}
	}

	masked, n := Mask(diff.String())
	p.Diff, p.Masked = masked, n
	for path, content := range p.Files {
		m, c := Mask(content)
		p.Files[path] = m
		p.Masked += c
	}
	if len(p.Diff) > MaxDiffBytes {
		return Payload{}, fmt.Errorf("the diff is %d bytes, over the limit of %d bytes (1.5 MiB); narrow it with paths, for example: taufinity review src/api/", len(p.Diff), MaxDiffBytes)
	}
	return p, nil
}

// blob is a file's content as far as classification needs it: the whole
// content when it is within the per-file limit, else only its head.
type blob struct {
	size int64
	data []byte
}

func collectDiff(ctx context.Context, dir, top, mergeBase string, o Options, p *Payload, diff *strings.Builder) error {
	var tl tally
	// What mergeBase is compared with: a commit, the index or the working tree.
	var against []string
	switch {
	case o.Head != "":
		against = []string{mergeBase, o.Head}
	case o.Staged:
		against = []string{"--cached", mergeBase}
	default:
		against = []string{mergeBase}
	}
	nameArgs := []string{"diff", "--name-status", "-z", "--no-renames", "--no-ext-diff"}
	nameArgs = append(nameArgs, against...)
	nameArgs = append(nameArgs, "--")
	nameArgs = append(nameArgs, o.Paths...)
	out, err := git(ctx, dir, nameArgs...)
	if err != nil {
		return err
	}
	type change struct {
		path    string
		deleted bool
	}
	var changes []change
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		changes = append(changes, change{path: fields[i+1], deleted: strings.HasPrefix(fields[i], "D")})
	}

	var tracked []string
	skip := func(path, reason string) { p.Skipped = append(p.Skipped, Skipped{Path: path, Reason: reason}) }
	for _, c := range changes {
		var b blob
		var rerr error
		switch {
		case excludedPath(c.path):
			skip(c.path, ReasonExcluded)
			continue
		case c.deleted:
			b, rerr = gitBlob(ctx, dir, mergeBase+":"+c.path)
		case o.Head != "":
			b, rerr = gitBlob(ctx, dir, o.Head+":"+c.path)
		case o.Staged:
			b, rerr = gitBlob(ctx, dir, ":"+c.path)
		default:
			var special bool
			b, special, rerr = diskBlob(filepath.Join(top, filepath.FromSlash(c.path)))
			if special {
				skip(c.path, ReasonExcluded)
				continue
			}
		}
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue // gone since git looked
			}
			return rerr
		}
		if reason := skipReason(c.path, b.size, b.data); reason != "" {
			skip(c.path, reason)
			continue
		}
		tracked = append(tracked, c.path)
		if !c.deleted {
			p.Files[c.path] = string(b.data)
			tl.files += len(b.data)
			if err := tl.check(); err != nil {
				return err
			}
		}
	}

	sort.Strings(tracked)
	for start := 0; start < len(tracked); start += pathChunk {
		end := min(start+pathChunk, len(tracked))
		args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
			"--src-prefix=a/", "--dst-prefix=b/"}
		args = append(args, against...)
		args = append(args, "--")
		for _, t := range tracked[start:end] {
			args = append(args, ":(top,literal)"+t)
		}
		d, err := git(ctx, dir, args...)
		if err != nil {
			return err
		}
		diff.Write(d)
		tl.diff = diff.Len()
		if err := tl.check(); err != nil {
			return err
		}
	}

	if o.Staged || o.Head != "" {
		return nil // untracked files are in neither the index nor a commit
	}
	untracked, err := listFiles(ctx, dir, o.Paths, "--others")
	if err != nil {
		return err
	}
	return appendNewFiles(top, untracked, p, diff, tl)
}

func collectWhole(ctx context.Context, dir, top string, o Options, p *Payload, diff *strings.Builder) error {
	files, err := listFiles(ctx, dir, o.Paths, "--cached", "--others")
	if err != nil {
		return err
	}
	return appendNewFiles(top, files, p, diff, tally{})
}

// listFiles returns sorted, de-duplicated repository-root relative paths of
// files git lists under the pathspecs, never ignored ones.
func listFiles(ctx context.Context, dir string, paths []string, which ...string) ([]string, error) {
	args := append([]string{"ls-files", "-z", "--full-name", "--exclude-standard"}, which...)
	args = append(args, "--")
	args = append(args, paths...)
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

// appendNewFiles adds each readable file from disk as a new-file diff and as
// an entry of Files, after the exclusions.
func appendNewFiles(top string, files []string, p *Payload, diff *strings.Builder, tl tally) error {
	for _, f := range files {
		if excludedPath(f) {
			// Before the read: an excluded file is never opened.
			p.Skipped = append(p.Skipped, Skipped{Path: f, Reason: ReasonExcluded})
			continue
		}
		b, special, err := diskBlob(filepath.Join(top, filepath.FromSlash(f)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // listed by the index but deleted from the working tree
		case err != nil:
			return err
		case special:
			if fi, lerr := os.Lstat(filepath.Join(top, filepath.FromSlash(f))); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
				p.Skipped = append(p.Skipped, Skipped{Path: f, Reason: ReasonExcluded})
			}
			continue // directories (submodules) are not files
		}
		if reason := skipReason(f, b.size, b.data); reason != "" {
			p.Skipped = append(p.Skipped, Skipped{Path: f, Reason: reason})
			continue
		}
		content := string(b.data)
		p.Files[f] = content
		diff.WriteString(newFileDiff(f, content))
		tl.files += len(content)
		tl.diff = diff.Len()
		if err := tl.check(); err != nil {
			return err
		}
	}
	return nil
}

// diskBlob reads a working tree file. special is true for anything that is
// not a regular file (a symlink could point outside the repository).
func diskBlob(path string) (blob, bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return blob{}, false, err
	}
	if !fi.Mode().IsRegular() {
		return blob{}, true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return blob{}, false, err
	}
	defer f.Close()
	limit := fi.Size()
	if limit > MaxFileBytes {
		limit = binaryProbe
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return blob{}, false, err
	}
	return blob{size: fi.Size(), data: data}, false, nil
}

// gitBlob reads an object (<rev>:<path>, or :<path> for the index).
func gitBlob(ctx context.Context, dir, spec string) (blob, error) {
	sizeOut, err := git(ctx, dir, "cat-file", "-s", spec)
	if err != nil {
		return blob{}, os.ErrNotExist
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	if err != nil {
		return blob{}, err
	}
	if size > MaxFileBytes {
		return blob{size: size}, nil // too large: no need to read it
	}
	data, err := git(ctx, dir, "cat-file", "blob", spec)
	if err != nil {
		return blob{}, err
	}
	return blob{size: size, data: data}, nil
}

// newFileDiff renders content as the unified diff of a file added to the tree.
func newFileDiff(path, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n", path, path, path)
	if content == "" {
		return b.String()
	}
	noEOL := !strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) == 1 {
		b.WriteString("@@ -0,0 +1 @@\n")
	} else {
		fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
	}
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	if noEOL {
		b.WriteString("\\ No newline at end of file\n")
	}
	return b.String()
}
