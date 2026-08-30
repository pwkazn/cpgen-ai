package securefs

import (
	"fmt"
	"os"
	"strings"

	"cpgen/internal/domain"
)

// OpenRegular opens path beneath root, verifies every traversed directory and
// the final regular file against the exact handles that will remain in use,
// and optionally requires the file to have exactly one hard link.
func OpenRegular(root *os.Root, safePath domain.SafeRelPath, requireSingleLink bool) (*os.File, os.FileInfo, error) {
	if root == nil {
		return nil, nil, fmt.Errorf("filesystem root is required")
	}
	if err := safePath.Validate(); err != nil {
		return nil, nil, err
	}
	parts := strings.Split(string(safePath), "/")
	current := root
	children := make([]*os.Root, 0, len(parts)-1)
	defer func() {
		for index := len(children) - 1; index >= 0; index-- {
			_ = children[index].Close()
		}
	}()

	for _, component := range parts[:len(parts)-1] {
		observed, err := current.Lstat(component)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect directory component %q: %w", component, err)
		}
		if observed.Mode()&os.ModeSymlink != 0 || !observed.IsDir() {
			return nil, nil, fmt.Errorf("path component %q is not a direct directory", component)
		}
		child, err := current.OpenRoot(component)
		if err != nil {
			return nil, nil, fmt.Errorf("open directory component %q: %w", component, err)
		}
		children = append(children, child)
		opened, err := child.Stat(".")
		if err != nil {
			return nil, nil, fmt.Errorf("stat opened directory component %q: %w", component, err)
		}
		if !os.SameFile(observed, opened) {
			return nil, nil, fmt.Errorf("directory component %q changed while opening", component)
		}
		current = child
	}

	name := parts[len(parts)-1]
	observed, err := current.Lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect file %q: %w", safePath, err)
	}
	if observed.Mode()&os.ModeSymlink != 0 || !observed.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("path %q is not a direct regular file", safePath)
	}
	file, err := current.Open(name)
	if err != nil {
		return nil, nil, fmt.Errorf("open file %q: %w", safePath, err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("stat opened file %q: %w", safePath, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(observed, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file %q changed while opening", safePath)
	}
	if requireSingleLink {
		single, err := hasSingleLink(file, opened)
		if err != nil {
			_ = file.Close()
			return nil, nil, fmt.Errorf("read link count for %q: %w", safePath, err)
		}
		if !single {
			_ = file.Close()
			return nil, nil, fmt.Errorf("file %q has more than one hard link", safePath)
		}
	}
	return file, opened, nil
}
