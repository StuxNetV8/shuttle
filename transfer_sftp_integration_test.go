//go:build go1.11
// +build go1.11

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Run the real SSH/SFTP client against a local server, without lab credentials.
func startTestSFTP(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					if channel.ChannelType() != "session" {
						channel.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					stream, requests, err := channel.Accept()
					if err != nil {
						return
					}
					go func() {
						for request := range requests {
							valid := request.Type == "subsystem" && string(request.Payload) == string(ssh.Marshal(struct{ Name string }{"sftp"}))
							request.Reply(valid, nil)
						}
					}()
					server, err := sftp.NewServer(stream)
					if err != nil {
						stream.Close()
						return
					}
					_ = server.Serve()
					_ = server.Close()
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func TestSFTPFileModeEndToEnd(t *testing.T) {
	host := startTestSFTP(t)
	root := t.TempDir()
	queue, remote := filepath.Join(root, "queue"), filepath.Join(root, "remote")
	for _, rel := range []string{"deviceA/run/result.txt", "deviceB/run/result.txt"} {
		file := filepath.Join(queue, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(rel), 0644); err != nil {
			t.Fatal(err)
		}
	}
	manager := &TransferManagerSftp{args: &Args{name: "test-client", sendType: "file"}, DestCredentials: DestCredentials{srcDir: queue, user: "test", dst: url.URL{Host: host, Path: filepath.ToSlash(remote)}}}
	transferPass(manager, queue, "file")
	for _, rel := range []string{"deviceA/run/result.txt", "deviceB/run/result.txt"} {
		data, err := os.ReadFile(filepath.Join(remote, filepath.FromSlash(rel)))
		if err != nil || string(data) != rel {
			t.Fatalf("remote %q: data=%q error=%v", rel, data, err)
		}
		if _, err := os.Stat(filepath.Join(queue, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("queue file retained: %v", err)
		}
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(queue, "failed.txt")
	if err := os.WriteFile(local, []byte("retry me"), 0644); err != nil {
		t.Fatal(err)
	}
	manager.dst.Path = filepath.ToSlash(filepath.Join(blocked, "child"))
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := manager.send_file(local, info)
	if ok || err == nil {
		t.Fatalf("expected remote path error: ok=%t error=%v", ok, err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("failed local file lost: %v", err)
	}
}
