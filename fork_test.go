package main

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileModePreservesPathsThroughStaging(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	oldPre, oldTemp := PreTempPath, TempPath
	PreTempPath, TempPath = filepath.Join(root, "pre"), filepath.Join(root, "queue")
	defer func() { PreTempPath, TempPath = oldPre, oldTemp }()
	if err := os.MkdirAll(TempPath, 0755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"deviceA/run1/result.txt", "deviceB/run2/result.txt", "root.txt"} {
		filename := filepath.Join(source, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(rel), 0644); err != nil {
			t.Fatal(err)
		}
		staged, err := CopyPreTempFile(source, filename)
		if err != nil {
			t.Fatal(err)
		}
		if staged != filepath.Join(PreTempPath, filepath.FromSlash(rel)) {
			t.Fatalf("incorrect staged path %q", staged)
		}
	}
	if err := CopyTempDirectory(); err != nil {
		t.Fatal(err)
	}
	dest := DestCredentials{srcDir: TempPath, dst: url.URL{Path: "/incoming/lab"}}
	for _, rel := range []string{"deviceA/run1/result.txt", "deviceB/run2/result.txt", "root.txt"} {
		queued := filepath.Join(TempPath, filepath.FromSlash(rel))
		data, err := os.ReadFile(queued)
		if err != nil || string(data) != rel {
			t.Fatalf("queued %q: data=%q error=%v", rel, data, err)
		}
		remote, err := dest.remotePath(queued)
		if err != nil || remote != "/incoming/lab/"+rel {
			t.Fatalf("remote=%q error=%v", remote, err)
		}
	}
}

func TestConvertedTransferUsesOwnRoot(t *testing.T) {
	root := t.TempDir()
	dest := DestCredentials{srcDir: filepath.Join(root, "converted"), dst: url.URL{Path: "/converted"}}
	remote, err := dest.remotePath(filepath.Join(dest.srcDir, "run", "result.zip"))
	if err != nil || remote != "/converted/run/result.zip" {
		t.Fatalf("remote=%q error=%v", remote, err)
	}
	if _, err := dest.remotePath(filepath.Join(root, "outside.txt")); err == nil {
		t.Fatal("outside path accepted")
	}
}

type failingTransfer struct {
	calls        []string
	fail         bool
	inconsistent bool
}

func (m *failingTransfer) doWork(quit chan int)     {}
func (m *failingTransfer) connect_to_server() error { return nil }
func (m *failingTransfer) send_file(filename string, _ os.FileInfo) (bool, error) {
	m.calls = append(m.calls, filepath.Base(filename))
	if m.fail && strings.HasPrefix(filepath.Base(filename), "a") {
		return m.inconsistent, fmt.Errorf("permission denied")
	}
	return true, nil
}

func TestFailedTransferRetainedAndOtherFilesContinue(t *testing.T) {
	for _, inconsistent := range []bool{false, true} {
		t.Run(fmt.Sprint(inconsistent), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "run", "nested")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a-failed.txt", "b-good.txt"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			oldInfo, oldError := InfoLogger, ErrorLogger
			InfoLogger, ErrorLogger = log.New(&output, "", 0), log.New(&output, "", 0)
			defer func() { InfoLogger, ErrorLogger = oldInfo, oldError }()
			manager := &failingTransfer{fail: true, inconsistent: inconsistent}
			transferPass(manager, root, "file")
			if len(manager.calls) != 2 {
				t.Fatalf("expected one attempt per file, got %v", manager.calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "a-failed.txt")); err != nil {
				t.Fatalf("failed file lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "b-good.txt")); !os.IsNotExist(err) {
				t.Fatalf("successful file retained: %v", err)
			}
			if !strings.Contains(output.String(), "permission denied") || !strings.Contains(output.String(), "a-failed.txt") {
				t.Fatalf("missing diagnostic: %s", output.String())
			}
			manager.fail = false
			transferPass(manager, root, "file")
			if _, err := os.Stat(filepath.Join(root, "run")); !os.IsNotExist(err) {
				t.Fatalf("empty directory retained: %v", err)
			}
		})
	}
}

func TestUnreadableSourceDoesNotTruncateDestination(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "existing")
	if err := os.WriteFile(dst, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Copy(filepath.Join(root, "missing"), dst); err == nil {
		t.Fatal("expected source open failure")
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing copy modified: %q %v", data, err)
	}
}

func TestFailedCopyDoesNotReplaceQueuedFile(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "existing")
	if err := os.WriteFile(dst, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Copy(root, dst); err == nil {
		t.Fatal("expected failure when reading a directory as a file")
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing copy modified: %q %v", data, err)
	}
}

func TestArchiveRetryRetainsSourceUntilSuccess(t *testing.T) {
	for _, mode := range []string{"zip", "tar"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "a-run")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("result"), 0644); err != nil {
				t.Fatal(err)
			}
			manager := &failingTransfer{fail: true}
			transferPass(manager, root, mode)
			if _, err := os.Stat(filepath.Join(dir, "result.txt")); err != nil {
				t.Fatalf("source lost after failure: %v", err)
			}
			manager.fail = false
			manager.calls = nil
			transferPass(manager, root, mode)
			if len(manager.calls) != 1 {
				t.Fatalf("archive attempted multiple times: %v", manager.calls)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("queue not cleared: %v %v", entries, err)
			}
		})
	}
}
