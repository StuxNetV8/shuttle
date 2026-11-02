package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/user"
	"path"
	"time"
)

var (
	InfoLogger        = log.New(os.Stdout, "INFO: ", log.LstdFlags)
	ErrorLogger       = log.New(os.Stderr, "ERROR: ", log.LstdFlags)
	args              Args
	tr                *http.Transport = nil
	LogPath           string
	TempPath          string
	TempConvertedPath string
	PreTempPath       string
	FlatTarTempPath   string
	PreScriptPath     string
)

// init initializes the logger and parses CMD args.
func initTool() {
	args = GetCmdArgs()

	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	wd := usr.HomeDir
	shuttleFolderName := fmt.Sprintf("shuttle_%s", args.name)
	mainShuttleFolderName := path.Join(wd, "shuttle")
	shuttleFolderName = path.Join(mainShuttleFolderName, shuttleFolderName)
	TempPath = path.Join(shuttleFolderName, "shuttle_temp")
	TempConvertedPath = path.Join(shuttleFolderName, "shuttle_converted_temp")
	PreTempPath = path.Join(shuttleFolderName, "shuttle_pre_temp")
	FlatTarTempPath = path.Join(shuttleFolderName, "shuttle_flat_tar_temp")
	PreScriptPath = path.Join(mainShuttleFolderName, "scripts")

	newPaths := []string{TempPath, PreTempPath, PreScriptPath}
	if args.sendType == "flat_tar" {
		newPaths = append(newPaths, FlatTarTempPath)
	}
	if args.preconvert {
		newPaths = append(newPaths, TempConvertedPath)
	}
	for _, newPath := range newPaths {
		if err := os.MkdirAll(newPath, os.ModePerm); err != nil {
			ErrorLogger.Println(err)
			panic("")
		}
	}

	LogPath = path.Join(shuttleFolderName, "shuttle_log.txt")

	logFile, err := os.OpenFile(LogPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
	if err != nil {
		panic(err)
	}
	mw := io.MultiWriter(os.Stdout, logFile)

	InfoLogger = log.New(mw, "-> INFO: ", log.Ldate|log.Ltime)
	ErrorLogger = log.New(mw, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)

	// Read in the cert file
}

func initArgs() {
	isCert := len(args.crt) > 0

	// Get the SystemCertPool, continue with an empty pool on error
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if isCert {
		certs, err := ReadFileCompat(args.crt)
		if err == nil {
			if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
				ErrorLogger.Println("No certs appended, using system certs only")
			}
		}
	}

	// Trust the augmented cert pool in our client
	config := &tls.Config{
		InsecureSkipVerify: true,
		RootCAs:            rootCAs,
	}

	tr = &http.Transport{TLSClientConfig: config}

}

// main starts the ELN file watcher. See README for more information.
func main() {
	initTool()
	initArgs()
	now := time.Now()
	InfoLogger.Println("Starting at ", now.Format(time.RFC822))
	defer (func() {
		now := time.Now()
		InfoLogger.Println("Done at ", now.Format(time.RFC822))
	})()

	// Chain as communication channel between the file watcher and the transfer manager
	done_files := make(chan string, 20)
	// For potential (not jet implemented quit conditions)
	quit := make(chan int)
	InfoLogger.Print(args.LogString())
	pm := newProcessManager(&args, done_files)
	go pm.doWork(quit)

	prm := newPrepareManager(&args, done_files)
	go prm.doWork(quit)

	tm := newFileTransferManager(&args)
	if err := tm.connect_to_server(); err != nil {
		ErrorLogger.Printf("Initial connection failed: error=%v; queued files will retry on transfer scans", err)
	}
	go tm.doWork(quit)

	if args.preconvert {
		tmc := newConvertedTransferManager(&args)
		if err := tmc.connect_to_server(); err != nil {
			ErrorLogger.Printf("Initial converter connection failed: error=%v; queued files will retry on transfer scans", err)
		}
		go tmc.doWork(quit)
	}

	for {
		time.Sleep(args.duration * 20)
	}
}
