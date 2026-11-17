// SPDX-License-Identifier: Apache-2.0

package s3blob_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a minimal path-style S3 server (objects, copy, multipart) that
// keeps data on disk so it does not distort heap measurements.
type fakeS3 struct {
	dir string
	mu  sync.Mutex
	seq int
}

func newFakeS3(t *testing.T) *httptest.Server {
	f := &fakeS3{dir: t.TempDir()}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeS3) objPath(p string) string {
	return filepath.Join(f.dir, "o", strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "__"))
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := r.URL.Path
	_ = os.MkdirAll(filepath.Join(f.dir, "o"), 0o750) //nolint:gosec // test-controlled path
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.seq++
		id := strconv.Itoa(f.seq)
		f.mu.Unlock()
		_ = os.MkdirAll(filepath.Join(f.dir, "mp", id), 0o750) //nolint:gosec // test-controlled path
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.write(w, r.Body, filepath.Join(f.dir, "mp", q.Get("uploadId"), fmt.Sprintf("%06d", n)))
		w.Header().Set("ETag", `"p`+strconv.Itoa(n)+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		dir := filepath.Join(f.dir, "mp", q.Get("uploadId"))
		names, _ := filepath.Glob(filepath.Join(dir, "*"))
		sort.Strings(names)
		out, err := os.Create(f.objPath(path)) //nolint:gosec // test-controlled path
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for _, n := range names {
			in, _ := os.Open(n) //nolint:gosec // test-controlled path
			_, _ = io.Copy(out, in)
			_ = in.Close()
		}
		_ = out.Close()
		_ = os.RemoveAll(dir) //nolint:gosec // test-controlled path
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
			ETag    string
		}{ETag: `"done"`})
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		_ = os.RemoveAll(filepath.Join(f.dir, "mp", q.Get("uploadId"))) //nolint:gosec // test-controlled path
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		src := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/")
		in, err := os.Open(f.objPath(src)) //nolint:gosec // test-controlled path
		if err != nil {
			f.noKey(w)
			return
		}
		defer func() { _ = in.Close() }()
		f.write(w, in, f.objPath(path))
		_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"c"</ETag></CopyObjectResult>`)
	case r.Method == http.MethodPut:
		f.write(w, r.Body, f.objPath(path))
		w.Header().Set("ETag", `"x"`)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		in, err := os.Open(f.objPath(path)) //nolint:gosec // test-controlled path
		if err != nil {
			f.noKey(w)
			return
		}
		defer func() { _ = in.Close() }()
		st, _ := in.Stat()
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, in)
		}
	case r.Method == http.MethodDelete:
		_ = os.Remove(f.objPath(path)) //nolint:gosec // test-controlled path
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}
}

func (f *fakeS3) write(w http.ResponseWriter, body io.Reader, dst string) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil { //nolint:gosec // test-controlled path
		http.Error(w, err.Error(), 500)
		return
	}
	out, err := os.Create(dst) //nolint:gosec // test-controlled path
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer func() { _ = out.Close() }()
	_, _ = io.Copy(out, body)
}

func (f *fakeS3) noKey(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
}
