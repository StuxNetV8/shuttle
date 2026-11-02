package main

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type DestCredentials struct {
	user, srcDir, pass string
	dst                url.URL
}

// TransferManager reacts on the channel done_files.
// If folder of file is ready to send it sends it via WebDAV (HTTP) to <CMD arg -dst>.
// It also initializes the zipping if <CMD arg -zip> is set.
type TransferManager interface {
	// doWork runs in a endless loop. It reacts on the channel done_files.
	// If folder of file is ready to send it sends it via HWebDAV (HTTP) to <CMD arg -dst>.
	// It also initializes the zipping if <CMD arg -zip> is set
	// It terminates as soon as a value is pushed into quit. Run in extra goroutine.
	doWork(quit chan int)

	connect_to_server() error

	// send_file sends a file via WebDAV
	send_file(path_to_file string, file os.FileInfo) (bool, error)
}

// relativeTransferPath keeps the spool-relative directory structure for every mode.
// Remote paths always use slashes, even when the client runs on Windows.
func relativeTransferPath(root, filename string) (string, error) {
	rel, err := filepath.Rel(root, filename)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("file %q is outside transfer root %q", filename, root)
	}
	return filepath.ToSlash(rel), nil
}

func (d DestCredentials) remotePath(filename string) (string, error) {
	rel, err := relativeTransferPath(d.srcDir, filename)
	if err != nil {
		return "", err
	}
	return path.Join(d.dst.Path, rel), nil
}

// transferFile only removes the local spool copy after confirmed success.
func transferFile(m TransferManager, filename string, info os.FileInfo) bool {
	started := time.Now()
	InfoLogger.Printf("Transfer started: source=%q bytes=%d", filename, info.Size())
	ok, err := m.send_file(filename, info)
	if err != nil || !ok {
		ErrorLogger.Printf("Transfer failed: source=%q bytes=%d elapsed=%s error=%v confirmed=%t; local copy retained, retry on next scan", filename, info.Size(), time.Since(started), err, ok)
		return false
	}
	InfoLogger.Printf("Transfer completed: source=%q bytes=%d elapsed=%s", filename, info.Size(), time.Since(started))
	if err := os.Remove(filename); err != nil {
		ErrorLogger.Printf("Local cleanup failed after successful transfer: path=%q error=%v; may be transferred again", filename, err)
		return false
	}
	return true
}

// transferPass makes one bounded attempt per file. A failed file must neither
// block later files nor cause deletion of an untransferred sibling.
func transferPass(m TransferManager, srcDir, sendType string) {
	spoolMutex.Lock()
	defer spoolMutex.Unlock()
	items, err := ReadDirCompat(srcDir)
	if err != nil {
		ErrorLogger.Printf("Cannot scan transfer directory: path=%q error=%v; retry on next scan", srcDir, err)
		return
	}
	// Archives with a source directory are rebuilt and handled with that directory.
	// Do not also attempt the stale archive entry from this scan's snapshot.
	archiveSources := make(map[string]bool)
	if sendType == "zip" || sendType == "tar" {
		suffix := ".zip"
		if sendType == "tar" {
			suffix = ".tar.gz"
		}
		for _, item := range items {
			if item.IsDir() {
				archiveSources[item.Name()+suffix] = true
			}
		}
	}
	for _, item := range items {
		if archiveSources[item.Name()] {
			continue
		}
		filename := filepath.Join(srcDir, item.Name())
		if !item.IsDir() {
			transferFile(m, filename, item)
			continue
		}
		if sendType == "zip" || sendType == "tar" {
			var archive string
			var err error
			if sendType == "zip" {
				archive, err = zipFolder(filename)
			} else {
				archive, err = tarFolder(filename)
			}
			if err != nil {
				ErrorLogger.Printf("Cannot create archive: source=%q error=%v; retry on next scan", filename, err)
				// Never send an incomplete archive during the next pass.
				if archive == "" {
					archive = filename + ".zip"
					if sendType == "tar" {
						archive = filename + ".tar.gz"
					}
				}
				if removeErr := os.Remove(archive); removeErr != nil && !os.IsNotExist(removeErr) {
					ErrorLogger.Printf("Cannot remove incomplete archive: path=%q error=%v", archive, removeErr)
				}
				continue
			}
			info, err := os.Stat(archive)
			if err == nil && transferFile(m, archive, info) {
				if err := os.RemoveAll(filename); err != nil {
					ErrorLogger.Printf("Archive source cleanup failed: path=%q error=%v", filename, err)
				}
			} else if err != nil {
				ErrorLogger.Printf("Cannot stat archive: path=%q error=%v", archive, err)
			}
			continue
		}
		err = filepath.Walk(filename, func(file string, info os.FileInfo, err error) error {
			if err != nil {
				ErrorLogger.Printf("Cannot scan queued path: path=%q error=%v; retry on next scan", file, err)
				return nil
			}
			if !info.IsDir() {
				transferFile(m, file, info)
			}
			return nil
		})
		if err != nil {
			ErrorLogger.Printf("Cannot walk transfer directory: path=%q error=%v", filename, err)
		}
		removeEmptyDirectories(filename)
	}
}

func removeEmptyDirectories(root string) {
	entries, err := ReadDirCompat(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			removeEmptyDirectories(filepath.Join(root, entry.Name()))
		}
	}
	// Remove only empty directories; a failed transfer always stays queued.
	_ = os.Remove(root)
}

func doWorkImplementation(quit chan int, m TransferManager, srcDir string, sendType string, duration time.Duration) {
	if duration <= 0 {
		duration = time.Second
	}
	for {
		select {
		case <-quit:
			return
		default:
		}
		transferPass(m, srcDir, sendType)
		select {
		case <-quit:
			return
		case <-time.After(duration):
		}
	}
}

func newFileTransferManager(args *Args) TransferManager {
	dest := DestCredentials{
		user:   args.user,
		pass:   args.pass,
		dst:    args.dst,
		srcDir: TempPath,
	}

	return newTransferManager(args, &dest)
}

func newConvertedTransferManager(args *Args) TransferManager {
	dest := DestCredentials{
		user:   args.userConverter,
		pass:   args.passConverter,
		dst:    args.dstConverter,
		srcDir: TempConvertedPath,
	}

	return newTransferManager(args, &dest)
}

func newTransferManager(args *Args, dest *DestCredentials) TransferManager {
	if args.tType == "webdav" {
		return &TransferManagerWebdav{DestCredentials: *dest, args: args}
	} else if args.tType == "sftp" {
		return &TransferManagerSftp{DestCredentials: *dest, args: args}
	}

	panic("Transfer type is not implemented")
}
