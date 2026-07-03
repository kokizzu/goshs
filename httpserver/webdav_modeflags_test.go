package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// webdavModeTree builds a webroot with a file to protect (secret.txt) and an
// existing file a MOVE/COPY could clobber (victim.txt).
func webdavModeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOP-SECRET"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("VICTIM"), 0644))
	return dir
}

// davReq issues a WebDAV request (optionally with Destination/Overwrite headers)
// against the mode-flag-enforcing handler.
func davReq(t *testing.T, h http.Handler, method, target, dest, overwrite string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if dest != "" {
		r.Header.Set("Destination", "http://example.com"+dest)
	}
	if overwrite != "" {
		r.Header.Set("Overwrite", overwrite)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func fileContent(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}

func fileMissing(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return os.IsNotExist(err)
}

// MOVE renames (deletes) the source, so --no-delete must block it. Regression
// for the residual GHSA gap where MOVE bypassed --no-delete.
func TestWebdav_NoDelete_BlocksMove(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.NoDelete = true
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "MOVE", "/secret.txt", "/gone.txt", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "secret.txt"), "source must survive a blocked MOVE")
	require.True(t, fileMissing(t, dir, "gone.txt"), "destination must not be created")
}

// MOVE with Overwrite:T also RemoveAll()s the destination — --no-delete must
// block it before the victim is destroyed.
func TestWebdav_NoDelete_BlocksMoveOverwrite(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.NoDelete = true
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "MOVE", "/secret.txt", "/victim.txt", "T")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "VICTIM", fileContent(t, dir, "victim.txt"), "existing file must not be clobbered")
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "secret.txt"))
}

// COPY with Overwrite onto an existing destination RemoveAll()s it first, so
// --no-delete must block that case.
func TestWebdav_NoDelete_BlocksCopyOverwrite(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.NoDelete = true
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "COPY", "/secret.txt", "/victim.txt", "T")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "VICTIM", fileContent(t, dir, "victim.txt"), "existing file must not be clobbered")
}

// A COPY to a fresh path deletes nothing and must stay allowed under
// --no-delete, even though the Overwrite header defaults to on.
func TestWebdav_NoDelete_AllowsCopyToNewPath(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.NoDelete = true
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "COPY", "/secret.txt", "/copy.txt", "")
	require.Less(t, w.Code, http.StatusBadRequest, "non-overwriting COPY must be allowed")
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "secret.txt"), "source must be untouched")
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "copy.txt"))
}

// --upload-only forbids deletion too, so MOVE must be blocked there as well.
func TestWebdav_UploadOnly_BlocksMove(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.UploadOnly = true
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "MOVE", "/secret.txt", "/gone.txt", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "secret.txt"))
}

// --read-only blocks every mutating verb, including MOVE and COPY.
func TestWebdav_ReadOnly_BlocksMoveAndCopy(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.ReadOnly = true
	h := newWebdavTestHandler(fs)

	require.Equal(t, http.StatusForbidden, davReq(t, h, "MOVE", "/secret.txt", "/gone.txt", "").Code)
	require.Equal(t, http.StatusForbidden, davReq(t, h, "COPY", "/secret.txt", "/copy.txt", "").Code)
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "secret.txt"))
}

// Control: with no mode flags a MOVE works as usual — the source is renamed away
// and the destination created. Confirms the guard does not over-block.
func TestWebdav_Default_AllowsMove(t *testing.T) {
	dir := webdavModeTree(t)
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	h := newWebdavTestHandler(fs)

	w := davReq(t, h, "MOVE", "/secret.txt", "/gone.txt", "")
	require.Less(t, w.Code, http.StatusBadRequest)
	require.True(t, fileMissing(t, dir, "secret.txt"), "MOVE should remove the source")
	require.Equal(t, "TOP-SECRET", fileContent(t, dir, "gone.txt"))
}
