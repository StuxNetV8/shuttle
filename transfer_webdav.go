package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/StarmanMartin/gowebdav"
)

// TransferManagerWebdav reacts on the channel done_files.
// If folder of file is ready to send it sends it via WebDAV (HTTP) to <CMD arg -dst>.
// It also initializes the zipping if <CMD arg -zip> is set.
type TransferManagerWebdav struct {
	args *Args
	DestCredentials
	client *gowebdav.Client
}

// doWork runs in a endless loop. It reacts on the channel done_files.
// If folder of file is ready to send it sends it via HWebDAV (HTTP) to <CMD arg -dst>.
// It also initializes the zipping if <CMD arg -zip> is set
// It terminates as soon as a value is pushed into quit. Run in extra goroutine.
func (m *TransferManagerWebdav) doWork(quit chan int) {
	doWorkImplementation(quit, m, m.srcDir, m.args.sendType, m.args.duration)
}

func (m *TransferManagerWebdav) connect_to_server() error {
	user := m.user
	password := m.pass

	c := gowebdav.NewClient(m.dst.String(), user, password, tr)
	c.SetTimeout(10 * time.Second)
	if err := c.Connect(); err != nil {
		return err
	}
	m.client = c
	return nil
}

// send_file sends a file via WebDAV
func (m *TransferManagerWebdav) send_file(path_to_file string, file os.FileInfo) (bool, error) {
	var webdavFilePath, urlPathDir string

	err := m.connect_to_server()
	if err != nil {
		return false, err
	}
	webdavFilePath, err = relativeTransferPath(m.srcDir, path_to_file)
	if err != nil {
		return false, err
	}
	urlPathDir = path.Dir(webdavFilePath)
	InfoLogger.Println("Sending...", webdavFilePath)

	if urlPathDir != "." {
		err := m.client.MkdirAll(urlPathDir, 0755)
		if err != nil {
			return false, err
		}
	}

	bytes, err := ReadFileCompat(path_to_file)
	if err != nil {
		return false, err
	}

	defer func() {
		if r := recover(); r != nil {

			err = errors.New(fmt.Sprintf("%+v", r))
			ErrorLogger.Printf("WebDav Panic: %s\n", err)
		}
	}()
	err = m.client.Write(webdavFilePath, bytes, 0644)

	return err == nil, err
}
