package selfupdate

import (
	"bytes"
	"compress/gzip"
	"io"
)

func gzipReader(b []byte) (io.ReadCloser, error) {
	return gzip.NewReader(bytes.NewReader(b))
}
