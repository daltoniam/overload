package review

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const MaxArchiveBytes = 256 << 20

func ExtractArchive(ctx context.Context, archive io.Reader, destination string) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	zip, err := gzip.NewReader(io.LimitReader(archive, MaxArchiveBytes+1))
	if err != nil {
		return err
	}
	defer func() { _ = zip.Close() }()
	tarReader := tar.NewReader(zip)
	var total int64
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > 10000 {
			return errors.New("repository archive has too many entries")
		}
		parts := strings.SplitN(header.Name, "/", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		name := parts[1]
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !fs.ValidPath(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			if header.Size != 0 {
				return errors.New("archive link has unexpected content")
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > 4<<20 || total+header.Size > MaxArchiveBytes {
				return errors.New("repository archive exceeds file or total size limit")
			}
			total += header.Size
			if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
				return err
			}
			file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d", header.Typeflag)
		}
	}
}
