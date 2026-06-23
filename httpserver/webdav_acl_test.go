package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
)

// newWebdavTestHandler wires the ACL-aware FileSystem and wdGuard's enforcement
// exactly as httpserver.Start does for the "webdav" mode, minus the unrelated
// mode-flag and middleware layers.
func newWebdavTestHandler(fs *FileServer) http.Handler {
	wd := &webdav.Handler{
		FileSystem: fs.newWebdavFileSystem(),
		LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fs.webdavEnforceACL(w, r) {
			return
		}
		ctx := context.WithValue(r.Context(), webdavCtxKey{}, r)
		wd.ServeHTTP(w, r.WithContext(ctx))
	})
}

// webdavACLTree builds a webroot with a public file and a password-protected
// subtree (user:pass) carrying a .goshs config.
func webdavACLTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.txt"), []byte("pub"), 0644))

	protected := filepath.Join(dir, "protected")
	require.NoError(t, os.Mkdir(protected, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(protected, "secret.txt"), []byte("topsecret"), 0644))

	hash, err := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	require.NoError(t, err)
	acl := fmt.Sprintf(`{"auth":"user:%s"}`, string(hash))
	require.NoError(t, os.WriteFile(filepath.Join(protected, ".goshs"), []byte(acl), 0644))
	return dir
}

func propfind(t *testing.T, h http.Handler, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PROPFIND", path, nil)
	r.Header.Set("Depth", "1")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The .goshs config file (holding the bcrypt hash) must never be served.
func TestWebdav_GoshsFile_NotServed(t *testing.T) {
	fs, cleanup := newTestFileServer(t, webdavACLTree(t))
	defer cleanup()
	h := newWebdavTestHandler(fs)

	r := httptest.NewRequest(http.MethodGet, "/protected/.goshs", nil)
	r.Header.Set("Authorization", basicAuthHeader("user", "pass"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.NotContains(t, w.Body.String(), "$2a$", "bcrypt hash must not leak via WebDAV")
}

// A file inside a password-protected directory must require the directory's
// credentials over WebDAV, just like the HTTP server.
func TestWebdav_ProtectedFile_RequiresAuth(t *testing.T) {
	fs, cleanup := newTestFileServer(t, webdavACLTree(t))
	defer cleanup()
	h := newWebdavTestHandler(fs)

	// No credentials → 401 with a challenge.
	r := httptest.NewRequest(http.MethodGet, "/protected/secret.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.NotContains(t, w.Body.String(), "topsecret")

	// Correct credentials → content served.
	r = httptest.NewRequest(http.MethodGet, "/protected/secret.txt", nil)
	r.Header.Set("Authorization", basicAuthHeader("user", "pass"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "topsecret", w.Body.String())
}

// A block-listed file must not be served over WebDAV.
func TestWebdav_BlockedFile_NotServed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocked.txt"), []byte("nope"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".goshs"), []byte(`{"block":["blocked.txt"]}`), 0644))

	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	h := newWebdavTestHandler(fs)

	r := httptest.NewRequest(http.MethodGet, "/blocked.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.NotContains(t, w.Body.String(), "nope")
}

// A PROPFIND listing must hide protected subdirectories, block-listed entries,
// and the .goshs file from unauthorised callers.
func TestWebdav_Propfind_HidesProtectedSubtree(t *testing.T) {
	fs, cleanup := newTestFileServer(t, webdavACLTree(t))
	defer cleanup()
	h := newWebdavTestHandler(fs)

	w := propfind(t, h, "/", "")
	require.Equal(t, http.StatusMultiStatus, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "public.txt")
	require.NotContains(t, body, "protected", "protected dir must be hidden from unauthorised listing")
}

// With valid credentials the protected subtree appears, but the .goshs config
// is still never listed.
func TestWebdav_Propfind_WithCreds_ShowsSubtreeButNotGoshs(t *testing.T) {
	fs, cleanup := newTestFileServer(t, webdavACLTree(t))
	defer cleanup()
	h := newWebdavTestHandler(fs)

	auth := basicAuthHeader("user", "pass")

	rootListing := propfind(t, h, "/", auth)
	require.Equal(t, http.StatusMultiStatus, rootListing.Code)
	require.Contains(t, rootListing.Body.String(), "protected")

	dirListing := propfind(t, h, "/protected", auth)
	require.Equal(t, http.StatusMultiStatus, dirListing.Code)
	body := dirListing.Body.String()
	require.Contains(t, body, "secret.txt")
	require.NotContains(t, body, ".goshs", "ACL config must never appear in a listing")
}
