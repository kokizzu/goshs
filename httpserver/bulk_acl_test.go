package httpserver

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// zipEntryNames reads the zip archive in body and returns the list of file names
// it contains.
func zipEntryNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

// aclTree builds a webroot containing a public file and a password-protected
// subtree, returning the webroot path. The protected dir requires user:pass.
func aclTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.txt"), []byte("pub"), 0644))

	protected := filepath.Join(dir, "protected")
	require.NoError(t, os.Mkdir(protected, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(protected, "secret.txt"), []byte("secret"), 0644))

	hash, err := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	require.NoError(t, err)
	acl := fmt.Sprintf(`{"auth":"user:%s"}`, hash)
	require.NoError(t, os.WriteFile(filepath.Join(protected, ".goshs"), []byte(acl), 0644))
	return dir
}

// Selecting a protected directory directly must yield a clean 401, because the
// directory's own .goshs (found by starting the upward ACL walk at the directory
// itself) requires authentication.
func TestBulkDownload_ProtectedDirSelected_NoCreds(t *testing.T) {
	fs, _ := newTestFileServer(t, aclTree(t))
	r := httptest.NewRequest(http.MethodGet, "/?bulk&file=/protected", nil)
	w := httptest.NewRecorder()
	fs.bulkDownload(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// Regression for the ACL bypass: selecting the webroot (a parent of a protected
// subtree) without credentials must still produce an archive, but that archive
// must NOT leak files from the protected subtree, nor the .goshs config itself.
func TestBulkDownload_ParentSelected_SkipsProtectedSubtree(t *testing.T) {
	fs, _ := newTestFileServer(t, aclTree(t))
	r := httptest.NewRequest(http.MethodGet, "/?bulk&file=/", nil)
	w := httptest.NewRecorder()
	fs.bulkDownload(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	names := zipEntryNames(t, w.Body.Bytes())
	require.Contains(t, names, "public.txt")
	require.NotContains(t, names, "protected/secret.txt", "protected subtree must not be leaked without credentials")
	for _, n := range names {
		require.NotEqual(t, ".goshs", filepath.Base(n), "ACL config file must never be archived")
	}
}

// With valid credentials for the protected subtree, the same parent selection
// must include the protected file.
func TestBulkDownload_ParentSelected_WithCreds_IncludesProtected(t *testing.T) {
	fs, _ := newTestFileServer(t, aclTree(t))
	r := httptest.NewRequest(http.MethodGet, "/?bulk&file=/", nil)
	r.Header.Set("Authorization", basicAuthHeader("user", "pass"))
	w := httptest.NewRecorder()
	fs.bulkDownload(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	names := zipEntryNames(t, w.Body.Bytes())
	require.Contains(t, names, "public.txt")
	require.Contains(t, names, "protected/secret.txt")
}

// A block list in a nested .goshs must be honoured during the recursive walk,
// even when the selection is a parent directory.
func TestBulkDownload_ParentSelected_HonoursNestedBlockList(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "ok.txt"), []byte("ok"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "secret.txt"), []byte("no"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(sub, ".goshs"), []byte(`{"block":["secret.txt"]}`), 0644))

	fs, _ := newTestFileServer(t, dir)
	r := httptest.NewRequest(http.MethodGet, "/?bulk&file=/", nil)
	w := httptest.NewRecorder()
	fs.bulkDownload(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	names := zipEntryNames(t, w.Body.Bytes())
	require.Contains(t, names, "sub/ok.txt")
	require.NotContains(t, names, "sub/secret.txt", "block-listed file in nested .goshs must not be archived")
}
