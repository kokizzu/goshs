package httpserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Regression tests for GHSA-mhxc-hfx2-7w79: a DIRECTORY named .goshs made
// findSpecialFile fail with EISDIR, findEffectiveACL returned an empty ACL plus
// an error, and every caller carried on with that empty ACL — silently removing
// the inherited per-directory auth and block list for the whole subtree.

// poisonTree builds webroot/secret (auth admin:secret-pass) with a nested
// webroot/secret/poison/plan.txt and returns the webroot.
func poisonTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	require.NoError(t, os.MkdirAll(filepath.Join(secret, "poison"), 0755))
	hash, err := bcrypt.GenerateFromPassword([]byte("secret-pass"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(secret, ".goshs"),
		[]byte(fmt.Sprintf(`{"auth":"admin:%s","block":["hidden.txt"]}`, hash)), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(secret, "poison", "plan.txt"), []byte("TOP-SECRET"), 0644))
	return dir
}

func TestFindEffectiveACL_GoshsDirectoryDoesNotPoison(t *testing.T) {
	root := poisonTree(t)
	poison := filepath.Join(root, "secret", "poison")
	require.NoError(t, os.Mkdir(filepath.Join(poison, ".goshs"), 0755))

	fs := &FileServer{Webroot: root}
	acl, err := fs.findEffectiveACL(poison)
	require.NoError(t, err)
	require.NotEmpty(t, acl.Auth, "ancestor auth must survive a .goshs directory")
	require.Contains(t, acl.Block, "hidden.txt", "ancestor block list must survive a .goshs directory")
}

func TestFindEffectiveACL_FailsClosedOnUnreadableConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.Mkdir(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, ".goshs"), []byte(`{not json`), 0644))

	fs := &FileServer{Webroot: root}
	acl, err := fs.findEffectiveACL(sub)
	require.Error(t, err)
	require.Equal(t, denyAllAuth, acl.Auth, "a broken .goshs must deny, not grant")

	r := httptest.NewRequest(http.MethodGet, "/sub/x", nil)
	r.SetBasicAuth("admin", "anything")
	require.False(t, aclSatisfied(r, acl))
	require.False(t, NewProtocolACL(root).Allowed(filepath.Join(sub, "x")))
}

func TestFindEffectiveACL_DanglingGoshsSymlinkFailsClosed(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, ".goshs")))

	fs := &FileServer{Webroot: root}
	acl, err := fs.findEffectiveACL(root)
	require.Error(t, err)
	require.Equal(t, denyAllAuth, acl.Auth)
}

// A path that does not exist yet (e.g. the target of a nested mkdir) must still
// inherit its existing ancestors' ACL rather than resolving to an empty one.
func TestFindEffectiveACL_MissingDirInheritsAncestor(t *testing.T) {
	root := poisonTree(t)
	fs := &FileServer{Webroot: root}
	acl, err := fs.findEffectiveACL(filepath.Join(root, "secret", "new", "deeper"))
	require.NoError(t, err)
	require.NotEmpty(t, acl.Auth)
}

func TestHandleMkdir_RejectsGoshsDirectory(t *testing.T) {
	root := poisonTree(t)
	fs, cleanup := newTestFileServer(t, root)
	defer cleanup()

	for _, p := range []string{"/secret/poison/.goshs/", "/secret/poison/.GOSHS/", "/secret/.goshs/x/"} {
		r := httptest.NewRequest(http.MethodPost, p+"?mkdir", nil)
		r.SetBasicAuth("admin", "secret-pass")
		w := httptest.NewRecorder()
		fs.handleMkdir(w, r)
		require.Equal(t, http.StatusForbidden, w.Code, p)
	}
	entries, err := os.ReadDir(filepath.Join(root, "secret", "poison"))
	require.NoError(t, err)
	for _, e := range entries {
		require.NotEqual(t, ".goshs", e.Name())
	}
}

// End-to-end form of the advisory PoC: even with a .goshs directory planted on
// disk (by any means), the protected file still requires the credential.
func TestHandler_GoshsDirectoryDoesNotRemoveAuth(t *testing.T) {
	root := poisonTree(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "secret", "poison", ".goshs"), 0755))
	fs, cleanup := newTestFileServer(t, root)
	defer cleanup()

	r := httptest.NewRequest(http.MethodGet, "/secret/poison/plan.txt", nil)
	w := httptest.NewRecorder()
	fs.handler(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.NotContains(t, w.Body.String(), "TOP-SECRET")

	r = httptest.NewRequest(http.MethodGet, "/secret/poison/plan.txt", nil)
	r.SetBasicAuth("admin", "secret-pass")
	w = httptest.NewRecorder()
	fs.handler(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "TOP-SECRET")
}

func TestProtocolACL_DeniesGoshsPathComponents(t *testing.T) {
	root := t.TempDir()
	p := NewProtocolACL(root)
	require.False(t, p.Allowed(filepath.Join(root, ".goshs")))
	require.False(t, p.Allowed(filepath.Join(root, "a", ".Goshs", "x.txt")))
	require.True(t, p.Allowed(filepath.Join(root, "a", "x.txt")))
}

func TestProtocolACL_FilterDirEntries(t *testing.T) {
	root := poisonTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "public.txt"), []byte("ok"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".goshs"), []byte(`{"block":["blocked.txt"]}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blocked.txt"), []byte("no"), 0644))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var names []string
	for _, e := range NewProtocolACL(root).FilterDirEntries(root, entries) {
		names = append(names, e.Name())
	}
	require.Equal(t, []string{"public.txt"}, names)
}
