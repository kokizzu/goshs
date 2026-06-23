package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sendFile now delegates to http.ServeContent, which adds HTTP Range support
// (resumable / seekable downloads). These tests pin that behaviour.

func writeServeFile(t *testing.T, name string, content []byte) (*FileServer, *os.File) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0644))

	fs, cleanup := newTestFileServer(t, dir)
	t.Cleanup(cleanup)

	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return fs, f
}

// A full GET advertises Accept-Ranges and returns the whole body.
func TestSendFile_FullRequest_AdvertisesRanges(t *testing.T) {
	body := []byte("0123456789abcdef")
	fs, f := writeServeFile(t, "data.bin", body)

	r := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
	require.Equal(t, body, w.Body.Bytes())
}

// A ranged GET returns 206 Partial Content with the requested slice.
func TestSendFile_RangeRequest_PartialContent(t *testing.T) {
	body := []byte("0123456789abcdef") // 16 bytes
	fs, f := writeServeFile(t, "data.bin", body)

	r := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	r.Header.Set("Range", "bytes=4-7")
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, "bytes 4-7/16", w.Header().Get("Content-Range"))
	require.Equal(t, []byte("4567"), w.Body.Bytes())
}

// Resuming a download (open-ended range) returns the tail of the file.
func TestSendFile_RangeRequest_Resume(t *testing.T) {
	body := []byte("0123456789abcdef")
	fs, f := writeServeFile(t, "data.bin", body)

	r := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	r.Header.Set("Range", "bytes=10-")
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, body[10:], w.Body.Bytes())
}

// Range support also applies to the ?download path, alongside the attachment
// disposition.
func TestSendFile_Download_RangeAndDisposition(t *testing.T) {
	body := []byte("0123456789abcdef")
	fs, f := writeServeFile(t, "payload.bin", body)

	r := httptest.NewRequest(http.MethodGet, "/payload.bin?download", nil)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, []byte("0123"), w.Body.Bytes())
	require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
}

// An unsatisfiable range yields 416 rather than a bogus 200.
func TestSendFile_RangeRequest_Unsatisfiable(t *testing.T) {
	fs, f := writeServeFile(t, "data.bin", []byte("short"))

	r := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	r.Header.Set("Range", "bytes=9999-10000")
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
}

// A conditional request with a current If-Modified-Since returns 304.
func TestSendFile_NotModified(t *testing.T) {
	fs, f := writeServeFile(t, "data.bin", []byte("content"))

	r := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	r.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusNotModified, w.Code)
	require.True(t, bytes.Equal(w.Body.Bytes(), []byte{}), "304 must have an empty body")
}
