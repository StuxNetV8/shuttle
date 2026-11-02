package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProcessManager manges the file watching process.
// As soon as all files in a subdirectory of <CMD arg -src>
// (or a file directly in <CMD arg -src>) are
// not changed for almost exactly <CMD -duration> seconds,
// the subdirectory will be pushed into the channel 'done_files'.
type ProcessManager struct {
	args             *Args
	doneFiles        chan string
	doneFlatPrefixes map[string][]string
}

func (m ProcessManager) applyRegex(name string) ([]string, string) {
	res := m.args.commonRegex.FindStringSubmatch(name)
	if res == nil {
		res = make([]string, 1)
		res[0] = name + "_archive"
	}
	if len(res) > 1 {
		res = res[1:]
	}
	joinedRes := strings.Join(res, "___")
	if len(joinedRes) == 0 || joinedRes == name {
		joinedRes = name + "_archive"
	}

	return res, joinedRes
}

func (m ProcessManager) collectTarPrefixes() {
	entries, err := ReadDirCompat(FlatTarTempPath)
	if err != nil {
		log.Fatal(err)
	}
	for prefix := range m.doneFlatPrefixes {
		delete(m.doneFlatPrefixes, prefix)
	}

	for _, v := range entries {
		if v.IsDir() {
			continue
		}
		res, joinedRes := m.applyRegex(v.Name())
		m.doneFlatPrefixes[joinedRes] = res
	}
}

func (m ProcessManager) processTarPrefixes() {
	entries, err := ReadDirCompat(FlatTarTempPath)
	if err != nil {
		log.Fatal(err)
	}

	for hashPrefix, groups := range m.doneFlatPrefixes {
		dirName := filepath.Join(FlatTarTempPath, hashPrefix)
		err := os.MkdirAll(dirName, os.ModeDir|os.ModePerm)
		if err != nil {
			ErrorLogger.Println(err)
			continue
		}
		for _, v := range entries {
			if v.IsDir() {
				continue
			}

			res, _ := m.applyRegex(v.Name())

			if AreEqual(res, groups) {
				sourcePath := filepath.Join(FlatTarTempPath, v.Name())
				err := Copy(sourcePath, filepath.Join(dirName, v.Name()))
				if err != nil {
					ErrorLogger.Println(err)
				}
				err = os.Remove(sourcePath)
			}
		}

		tarPath, err := tarFolder(dirName)
		if err != nil {
			ErrorLogger.Println(err)
		}

		err = Copy(tarPath, filepath.Join(dirName, filepath.Base(tarPath)))
		if err != nil {
			ErrorLogger.Println(err)
		}
		m.doneFiles <- tarPath
		err = os.RemoveAll(dirName)
		if err != nil {
			ErrorLogger.Println(err)
			continue
		}

		delete(m.doneFlatPrefixes, hashPrefix)
	}
}

func (m ProcessManager) isFileLocked(path string) bool {
	err := os.Rename(path, path)
	return err != nil
}

// doWork runs in an endless loop. It watches the files in the <CMD arg -src> directory.
// It terminates as soon as a value is pushed into quit. Run in extra goroutine.
func (m ProcessManager) doWork(quit chan int) {
	InfoLogger.Println("Started watch process.")
	lockedFiles := make(map[string]struct{})
	for {
		select {
		case <-quit:
			return
		default:
			now := time.Now()
			doneFolders := make(map[string]bool)
			// Checking all files in <CMD arg -src>.
			err := filepath.Walk(m.args.src,
				func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if !info.IsDir() {
						modified := info.ModTime()
						diff := now.Sub(modified)
						if lockErr := os.Rename(path, path); lockErr != nil {
							if _, exists := lockedFiles[path]; !exists {
								ErrorLogger.Printf("File postponed: path=%q reason=lock-check-failed error=%v; possibly locked or inaccessible, checking again next scan", path, lockErr)
							}
							lockedFiles[path] = struct{}{}
						} else if _, exists := lockedFiles[path]; exists || diff < 2*m.args.duration {
							if exists {
								InfoLogger.Printf("File accessible again: path=%q", path)
								delete(lockedFiles, path)
							}
							if relPath, err := filepath.Rel(m.args.src, path); err == nil {
								folder := relPath
								if m.args.sendType != "file" && m.args.sendType != "flat_tar" {
									folder = getRootDir(relPath)
								}

								if _, ok := doneFolders[folder]; !ok {
									doneFolders[folder] = true
								}

								if diff <= m.args.duration {
									doneFolders[folder] = false
								}
							} else {
								ErrorLogger.Println(err)
							}
						}
					}
					return nil
				})

			if err != nil {
				ErrorLogger.Println(err)
			}

			// Pushing all complete subdirectory into done_files channel.
			if m.args.sendType == "flat_tar" {
				m.collectTarPrefixes()
			}

			for k, v := range doneFolders {
				if v {
					InfoLogger.Println("Folder/File ready to send: ", k)
					if m.args.sendType == "flat_tar" {
						src := filepath.Join(m.args.src, k)
						dst := filepath.Join(FlatTarTempPath, filepath.Base(k))
						err := Copy(src, dst)
						if err != nil {
							ErrorLogger.Println(err)
						}
					} else {
						m.doneFiles <- filepath.Join(m.args.src, k)
					}
				}
			}
			if m.args.sendType == "flat_tar" {
				m.processTarPrefixes()
			}

			time.Sleep(m.args.duration - time.Since(now))
		}
	}
}

// newProcessManager factory for ProcessManager struct
func newProcessManager(args *Args, doneFiles chan string) ProcessManager {
	return ProcessManager{args: args, doneFiles: doneFiles, doneFlatPrefixes: make(map[string][]string)}
}
