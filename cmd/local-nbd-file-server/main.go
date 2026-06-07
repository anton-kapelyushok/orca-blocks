package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/anton-k/orca-blocks/pkg/nbd"
)

type fileDevice struct {
	file *os.File
	size int64
}

func (d *fileDevice) Size() int64 {
	return d.size
}

func (d *fileDevice) ReadAt(_ context.Context, offset, length int64) ([]byte, error) {
	data := make([]byte, int(length))
	n, err := d.file.ReadAt(data, offset)
	if err != nil && n == 0 {
		return nil, err
	}
	return data[:n], nil
}

func (d *fileDevice) WriteAt(_ context.Context, offset int64, data []byte) error {
	_, err := d.file.WriteAt(data, offset)
	return err
}

func (d *fileDevice) Flush(context.Context) error {
	return d.file.Sync()
}

func (d *fileDevice) Disconnect(context.Context) error {
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:10909", "TCP address for the NBD server")
	path := flag.String("file", "", "file to expose as an NBD export")
	exportName := flag.String("export", "bench", "accepted NBD export name")
	verbose := flag.Bool("verbose", false, "log every NBD request")
	flag.Parse()

	if *path == "" {
		log.Fatal("-file is required")
	}
	file, err := os.OpenFile(*path, os.O_RDWR, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		log.Fatal(err)
	}
	device := &fileDevice{file: file, size: info.Size()}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var logger *log.Logger
	if *verbose {
		logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	}
	server := &nbd.Server{
		Resolve: func(name string) (nbd.Device, error) {
			if name != "" && name != *exportName {
				return nil, fmt.Errorf("unknown export %q", name)
			}
			return device, nil
		},
		Logger: logger,
	}

	log.Printf("serving file=%s size=%d addr=%s export=%s", *path, info.Size(), *addr, *exportName)
	if err := server.Serve(ctx, ln); err != nil {
		log.Fatal(err)
	}
}
