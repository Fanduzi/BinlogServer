// Package releaseprep reads release surfaces from the working tree or from one git commit.
// input: a repo directory and, for a tag check, a git ref
// output: file bodies for Check (one file, or every tracked file for the stale scan) and the repo's release tags
// pos: I/O side of the release-prep gate; checks live in releaseprep.go
// note: if this file changes, update this header and README.md.
package releaseprep

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Dir reads files from a working tree, so the check can run before committing.
type Dir string

// Read implements Source.
func (d Dir) Read(path string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(string(d), path))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// Files reads every tracked file in the working tree as stored. Symlinks are
// not followed (a link to a directory or outside the repo is not a file of
// the release); they are returned in links.
func (d Dir) Files() (files map[string]string, links []string, err error) {
	out, err := git(string(d), "ls-files", "-z")
	if err != nil {
		return nil, nil, err
	}
	files = map[string]string{}
	for _, p := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if p == "" {
			continue
		}
		full := filepath.Join(string(d), p)
		fi, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted in the working tree
		}
		if err != nil {
			return nil, nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, p)
			continue
		}
		if !fi.Mode().IsRegular() {
			continue // a submodule checkout
		}
		b, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		files[p] = string(b)
	}
	return files, links, nil
}

// GitRef reads files from one commit, which is what release.yml checks on a tag.
type GitRef struct {
	Dir, Ref string
}

// Read implements Source.
func (g GitRef) Read(path string) (string, bool, error) {
	if _, err := git(g.Dir, "cat-file", "-e", g.Ref+":"+path); err != nil {
		return "", false, nil
	}
	out, err := git(g.Dir, "cat-file", "blob", g.Ref+":"+path)
	return out, err == nil, err
}

// Files reads every file in the commit with one git cat-file --batch.
// Symlinks (mode 120000) are not followed; they are returned in links.
func (g GitRef) Files() (files map[string]string, links []string, err error) {
	out, err := git(g.Dir, "ls-tree", "-r", "-z", "--format=%(objectmode) %(objecttype) %(objectname)\t%(path)", g.Ref)
	if err != nil {
		return nil, nil, err
	}
	var paths, names []string
	for _, rec := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || f[1] != "blob" {
			continue // submodules
		}
		if f[0] == "120000" {
			links = append(links, p)
			continue
		}
		paths = append(paths, p)
		names = append(names, f[2])
	}
	cmd := exec.Command("git", "-C", g.Dir, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("git cat-file --batch: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	files = map[string]string{}
	buf := stdout.Bytes()
	for _, p := range paths {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, nil, fmt.Errorf("git cat-file --batch: short output at %s", p)
		}
		var sha, typ string
		var size int
		if _, err := fmt.Sscanf(string(buf[:nl]), "%s %s %d", &sha, &typ, &size); err != nil || nl+1+size > len(buf) {
			return nil, nil, fmt.Errorf("git cat-file --batch: bad header %q for %s", buf[:nl], p)
		}
		files[p] = string(buf[nl+1 : nl+1+size])
		buf = buf[nl+1+size+1:]
	}
	return files, links, nil
}

// ResolveRef fails when ref does not name a commit.
func ResolveRef(dir, ref string) error {
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return fmt.Errorf("unknown ref %q", ref)
	}
	return nil
}

// MergedTags lists the v* tags that are ancestors of ref (or ref itself).
func MergedTags(dir, ref string) ([]string, error) {
	out, err := git(dir, "tag", "--list", "--merged", ref, "v*")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
