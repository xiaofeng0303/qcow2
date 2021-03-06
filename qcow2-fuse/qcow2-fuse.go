package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/vasi/qcow2"
	"github.com/vasi/qcow2/eio"
	"golang.org/x/net/context"
)

type file struct {
	guest qcow2.Guest
}

func (f file) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0644
	a.Size = uint64(f.guest.Size())
	return nil
}

func (f file) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	n, err := f.guest.ReadAt(resp.Data[:req.Size], req.Offset)
	resp.Data = resp.Data[:n]
	if err != nil {
		log.Print(err)
	}
	return err
}

func (f file) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	n, err := f.guest.WriteAt(req.Data, req.Offset)
	if err != nil {
		log.Print(err)
	}
	resp.Size = n
	return err
}

func main() {
	filename := os.Args[1]

	dstname := filepath.Base(filename)
	if filepath.Ext(dstname) == ".qcow2" {
		dstname = strings.TrimSuffix(dstname, filepath.Ext(dstname))
	}

	f, err := os.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	q, err := qcow2.Open(f)
	if err != nil {
		eio.Trace(err)
		log.Fatal(err)
	}
	defer q.Close()

	guest, err := q.Guest()
	if err != nil {
		log.Fatal(err)
	}
	defer guest.Close()

	conn, err := fuse.Mount(
		os.Args[2],
		fuse.OSXFUSELocations(fuse.OSXFUSEPaths{
			DevicePrefix: "/dev/osxfuse",
			Load:         "/opt/local/Library/Filesystems/osxfuse.fs/Contents/Resources/load_osxfuse",
			Mount:        "/opt/local/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse",
			DaemonVar:    "MOUNT_OSXFUSE_DAEMON_PATH",
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	tree := &fs.Tree{}
	tree.Add(dstname, file{guest})
	err = fs.Serve(conn, tree)
	if err != nil {
		log.Fatal(err)
	}

	<-conn.Ready
	if err := conn.MountError; err != nil {
		log.Fatal(err)
	}
}
