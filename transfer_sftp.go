//go:build go1.11
// +build go1.11

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TransferManagerSftp reacts on the channel done_files.
// If folder of file is ready to send it sends it via WebDAV (HTTP) to <CMD arg -dst>.
// It also initializes the zipping if <CMD arg -zip> is set.
type TransferManagerSftp struct {
	args *Args
	DestCredentials
	conn   *ssh.Client
	client *sftp.Client
}

// doWork runs in a endless loop. It reacts on the channel done_files.
// If folder of file is ready to send it sends it via HWebDAV (HTTP) to <CMD arg -dst>.
// It also initializes the zipping if <CMD arg -zip> is set
// It terminates as soon as a value is pushed into quit. Run in extra goroutine.
func (m *TransferManagerSftp) doWork(quit chan int) {
	doWorkImplementation(quit, m, m.srcDir, m.args.sendType, m.args.duration)
}

func (m *TransferManagerSftp) connect_to_server() error {
	user := m.user
	password := m.pass
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		Timeout:         15 * time.Second,
		HostKeyCallback: ssh.HostKeyCallback(func(hostname string, remote net.Addr, key ssh.PublicKey) error { return nil }),
	}

	dst := m.dst.Host

	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	InfoLogger.Printf("SFTP connecting: client=%q host=%q user=%q", m.args.name, dst, user)

	conn, err := ssh.Dial("tcp", dst, config)
	if err != nil {
		return fmt.Errorf("SSH connect host=%q user=%q: %v", dst, user, err)
	}

	// create new SFTP client
	client, err := sftp.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SFTP session host=%q: %v", dst, err)
	}

	m.client = client
	m.conn = conn

	InfoLogger.Printf("SFTP connected: client=%q host=%q user=%q", m.args.name, dst, user)
	return nil
}

// send_file confirms success only after the remote file has closed successfully.
func (m *TransferManagerSftp) send_file(filename string, file os.FileInfo) (bool, error) {
	remote, err := m.DestCredentials.remotePath(filename)
	if err != nil {
		return false, err
	}
	src, err := os.Open(filename)
	if err != nil {
		return false, fmt.Errorf("open local file %q (possibly locked or inaccessible): %v", filename, err)
	}
	defer src.Close()
	if err := m.connect_to_server(); err != nil {
		return false, err
	}
	// Close every session, including failed transfers; next attempt reconnects.
	defer func() { _ = m.client.Close(); _ = m.conn.Close(); m.client = nil; m.conn = nil }()
	InfoLogger.Printf("SFTP upload: source=%q destination=%q host=%q bytes=%d", filename, remote, m.dst.Host, file.Size())
	if dir := path.Dir(remote); dir != "." {
		if err := m.client.MkdirAll(dir); err != nil {
			return false, fmt.Errorf("SFTP mkdir %q: %v", dir, err)
		}
	}
	dst, err := m.client.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return false, fmt.Errorf("SFTP open destination %q: %v", remote, err)
	}
	copied, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return false, fmt.Errorf("SFTP copy %q -> %q after %d bytes: %v", filename, remote, copied, copyErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("SFTP close destination %q after %d bytes: %v", remote, copied, closeErr)
	}
	InfoLogger.Printf("SFTP upload completed: destination=%q bytes=%d", remote, copied)
	return true, nil
}
