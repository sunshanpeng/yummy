//go:build unix

package sourcearchive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRejectLocalLinksAndSpecialFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(string) error
	}{
		{"符号链接", func(root string) error { return os.Symlink("Dockerfile", filepath.Join(root, "link")) }},
		{"越界符号链接", func(root string) error { return os.Symlink("/etc/passwd", filepath.Join(root, "link")) }},
		{"硬链接", func(root string) error {
			return os.Link(filepath.Join(root, "Dockerfile"), filepath.Join(root, "link"))
		}},
		{"FIFO", func(root string) error { return syscall.Mkfifo(filepath.Join(root, "fifo"), 0600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, options := fixture(t)
			if err := test.create(root); err != nil {
				t.Fatal(err)
			}
			if _, err := Preview(context.Background(), options); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("接受不支持的本地文件: %v", err)
			}
		})
	}
}

func TestRejectLinkedContextAndIgnore(t *testing.T) {
	root, options := fixture(t)
	if err := os.Symlink(".", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	options.Context = "alias"
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("允许链接context: %v", err)
	}
	options.Context = "."
	if err := os.Symlink("Dockerfile", filepath.Join(root, ".dockerignore")); err != nil {
		t.Fatal(err)
	}
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("允许链接ignore: %v", err)
	}
}

func TestRealRootAllowlistAndDestinationSymlink(t *testing.T) {
	root, options := fixture(t)
	other := t.TempDir()
	alias := filepath.Join(other, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	options.Root = alias
	options.AllowRoots = []string{other}
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("符号根逃逸授权: %v", err)
	}
	options.AllowRoots = []string{root}
	if _, err := Preview(context.Background(), options); err != nil {
		t.Fatalf("真实根在授权内应该允许: %v", err)
	}
	data, _ := archive(t, options)
	if _, err := Extract(context.Background(), bytes.NewReader(data), alias, ArchiveOptions{}); !errors.Is(err, ErrDestination) {
		t.Fatalf("接受链接目标: %v", err)
	}
}
