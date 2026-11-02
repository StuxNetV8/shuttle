package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Protect publication into the spool from concurrent scanning/upload.
var spoolMutex sync.Mutex

// getRootDir returns the root directory, i.e., the first directory in the path.
// If the path is relative <root>/../<file> it returns the relative root.
// If it is not relative it returns the absolut root,  i.e.: '/'
func getRootDir(path string) string {
	current := path
	for {
		path = filepath.Dir(path)
		if path == "." || path == "" || path == current {
			return current
		}
		current = path
	}
}

func RunPreScripts(srcFilePath string) {
	entries, err := ReadDirCompat(PreScriptPath)
	if err != nil {
		log.Fatal(err)
	}

	for _, file := range entries {
		runScript(file, PreScriptPath, srcFilePath)
	}
}

func exeDir() (string, error) {

	// DEV override
	if dir := os.Getenv("APP_BASE_DIR"); dir != "" {
		return dir, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func RunConverterScripts(srcFilePath string) {
	entries, err := exeDir()
	if err != nil {
		log.Fatal(err)
	}
	file := ""
	if runtime.GOOS == "windows" {
		file = filepath.Join(entries, "ChemConverterApp.exe")
	} else {
		file = filepath.Join(entries, "ChemConverterApp")
	}
	fileInfo, err := os.Stat(file)
	if err != nil {
		log.Fatal("Error Converter executable dose not exists:\n |->", file, "\n", err)
	}
	runScript(fileInfo, entries, "convert", "-i", srcFilePath, "-o", "jcampzip")

}

func runScript(file fs.FileInfo, cwd string, args ...string) {
	if !file.IsDir() && strings.Contains(".exe.sh.fish", filepath.Ext(file.Name())) {
		var (
			cmd *exec.Cmd
		)
		if runtime.GOOS == "windows" {
			absScriptPath := filepath.Join(cwd, file.Name())
			cmd = exec.Command(absScriptPath, args...)
		} else {
			cmd = exec.Command("./"+file.Name(), args...)
		}

		cmd.Dir = cwd
		var outb, errb bytes.Buffer
		cmd.Stdout = &outb
		cmd.Stderr = &errb
		err := cmd.Run()
		if err != nil {
			ErrorLogger.Println(err.Error())
		}
		InfoLogger.Println(file.Name(), "\n |-> out:", outb.String(), "\n |-> err:", errb.String())
	}
}

func AreEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CopyPreTempFile preserves the path relative to the configured source root.
func CopyPreTempFile(sourceRoot, filename string) (string, error) {
	rel, err := relativeTransferPath(sourceRoot, filename)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(PreTempPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", err
	}
	if err := Copy(filename, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func CopyPreTempDirectory(scrDir string) (string, error) {
	_ = os.MkdirAll(PreTempPath, os.ModePerm)
	newPath := filepath.Base(scrDir)
	err := copyDirectory(scrDir, PreTempPath)
	return filepath.Join(PreTempPath, newPath), err
}

func CopyTempDirectory() error {
	spoolMutex.Lock()
	defer spoolMutex.Unlock()
	entries, err := ReadDirCompat(PreTempPath)
	if err != nil {
		return err
	}

	for _, file := range entries {
		absTempPath := filepath.Join(PreTempPath, file.Name())
		err := copyDirectory(absTempPath, TempPath)
		if err != nil {
			return err
		}
	}

	return os.RemoveAll(PreTempPath)
}

func copyDirectory(scrDir string, destDir string) error {
	return filepath.Walk(scrDir, func(sourcePath string, fileInfo os.FileInfo, e error) error {
		if e != nil {
			return fmt.Errorf("scan source %q: %v", sourcePath, e)
		}
		destPath, err := filepath.Rel(filepath.Dir(scrDir), sourcePath)
		if err != nil {
			return err
		}
		destPath = filepath.Join(destDir, destPath)
		if fileInfo.IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
		} else {
			if err := Copy(sourcePath, destPath); err != nil {
				return err
			}
		}
		return nil
	})
}

func Copy(srcFile, dstFile string) error {
	in, err := os.Open(srcFile)
	if err != nil {
		return fmt.Errorf("open source %q (possibly locked or inaccessible): %v", srcFile, err)
	}
	defer in.Close()
	// Publish only a completely copied and closed file. Failed copies leave
	// an already queued file intact instead of truncating it.
	out, err := os.CreateTemp(filepath.Dir(dstFile), ".shuttle-copy-*")
	if err != nil {
		return fmt.Errorf("create staging file for %q: %v", dstFile, err)
	}
	temporary := out.Name()
	defer os.Remove(temporary)
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %q to %q: %v", srcFile, dstFile, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close staging file %q: %v", dstFile, closeErr)
	}
	if err := os.Rename(temporary, dstFile); err != nil {
		return fmt.Errorf("publish staging file %q: %v", dstFile, err)
	}
	return nil
}

// zipFolder zips a folder and safes the zipped folder with the same name in the same directory.
func zipFolder(path_src string) (string, error) {
	// Create a buffer to write our archive to.
	output_path := path_src + ".zip"
	outFile, err := os.Create(output_path)
	if err != nil {
		return "", err
	}
	defer func(outFile *os.File) {
		err := outFile.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(outFile)

	// Create a new zip archive.
	w := zip.NewWriter(outFile)
	defer func(w *zip.Writer) {
		err := w.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(w)
	err = filepath.Walk(path_src,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			rel_path, err := filepath.Rel(path_src, path)
			if err != nil {
				return err
			}

			dat, err := ReadFileCompat(path)
			if err != nil {
				return err
			}

			f, err := w.Create(rel_path)
			if err != nil {
				return err
			}
			_, err = f.Write([]byte(dat))

			return err

		})

	// Make sure to check the error on Close.

	if err != nil {
		return "", err
	}
	return output_path, nil
}

// tarFolder zips a folder and safes the zipped folder with the same name in the same directory.
func tarFolder(path_src string) (string, error) {
	// Create a buffer to write our archive to.
	output_path := path_src + ".tar.gz"
	outFile, err := os.Create(output_path)
	if err != nil {
		return "", err
	}
	defer func(outFile *os.File) {
		err := outFile.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(outFile)

	// Create new Writers for gzip and tar
	// These writers are chained. Writing to the tar writer will
	// write to the gzip writer which in turn will write to
	// the "buf" writer
	gw := gzip.NewWriter(outFile)
	defer func(gwEnd *gzip.Writer) {
		err := gwEnd.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(gw)
	tw := tar.NewWriter(gw)
	defer func(twEnd *tar.Writer) {
		err := twEnd.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(tw)

	err = filepath.Walk(path_src,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			rel_path, err := filepath.Rel(path_src, path)
			if err != nil {
				return err
			}

			file, err := os.Open(path)
			if err != nil {
				return err
			}

			defer func(file *os.File) {
				if file.Close() != nil {
					return
				}
			}(file)

			// Get FileInfo about our file providing file size, mode, etc.
			finfo, err := file.Stat()
			if err != nil {
				return err
			}

			// Create a tar Header from the FileInfo data
			header, err := tar.FileInfoHeader(finfo, finfo.Name())
			if err != nil {
				return err
			}

			// Use full path as name (FileInfoHeader only takes the basename)
			// If we don't do this the directory strucuture would
			// not be preserved
			// https://golang.org/src/archive/tar/common.go?#L626
			header.Name = rel_path

			// Write file header to the tar archive
			err = tw.WriteHeader(header)
			if err != nil {
				return err
			}

			// Copy file content to tar archive
			_, err = io.Copy(tw, file)
			if err != nil {
				return err
			}

			return nil

		})

	// Make sure to check the error on Close.

	if err != nil {
		return "", err
	}
	return output_path, nil
}

func AddLinkToZip(file, zipPath string) error {
	// 1️⃣ create temp file
	tmp, err := os.CreateTemp("", "link.txt")
	if err != nil {
		return err
	}

	// always clean up
	defer func(tmp *os.File) {
		err := tmp.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(tmp)
	defer func(name string) {
		err := os.Remove(name)
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(tmp.Name())

	// write content
	if _, err := tmp.WriteString(args.public + path.Base(file)); err != nil {
		return err
	}

	return AddFileToZip(zipPath, tmp.Name(), ".link")
}

func AddFileToZip(zipPath, filePath, fileName string) error {
	// Open existing zip
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer func(r *zip.ReadCloser) {
		err := r.Close()
		if err != nil {
			ErrorLogger.Println(err)
		}
	}(r)

	// Create temp zip
	tmpFile, err := os.CreateTemp("", "ziptmp-*.zip")
	if err != nil {
		return err
	}
	defer func(name string) {
		err := os.Remove(name)
		if err != nil {
		}
	}(tmpFile.Name())

	w := zip.NewWriter(tmpFile)

	defer func(w *zip.Writer) {
		err := w.Close()
		if err != nil {
		}
	}(w)

	// Copy existing files
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}

		hdr := f.FileHeader
		hdr.Method = zip.Deflate

		dst, err := w.CreateHeader(&hdr)
		if err != nil {
			errRc := rc.Close()
			if errRc != nil {
				ErrorLogger.Println(errRc)
			}
			return err
		}

		if _, err = io.Copy(dst, rc); err != nil {
			errRc := rc.Close()
			if errRc != nil {
				ErrorLogger.Println(errRc)
			}
			return err
		}
		errRc := rc.Close()
		if errRc != nil {
			return err
		}
	}

	// Add new file
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {

		}
	}(file)

	info, err := file.Stat()
	if err != nil {
		return err
	}

	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = fileName

	dst, err := w.CreateHeader(hdr)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dst, file); err != nil {
		return err
	}

	// Close zip writer
	if err := w.Close(); err != nil {
		return err
	}

	if err := tmpFile.Close(); err != nil {
		return err
	}

	// Replace original zip
	return os.Rename(tmpFile.Name(), zipPath)
}
