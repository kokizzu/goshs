package tftpserver

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for GHSA-q8gg-q2wc-w52g: TFTP served the webroot with no
// .goshs ACL enforcement, so auth-protected files, block-listed files and the
// .goshs file itself (bcrypt hash) were anonymously readable.

func aclTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	require.NoError(t, os.Mkdir(secret, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(secret, ".goshs"), []byte(`{"auth":"admin:$2a$04$abcdefghijklmnopqrstuu"}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(secret, "file.txt"), []byte("TOP-SECRET"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".goshs"), []byte(`{"block":["blocked.txt"]}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blocked.txt"), []byte("BLOCKED"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "public.txt"), []byte("public"), 0644))
	return root
}

func TestRRQDeniedByACL(t *testing.T) {
	root := aclTree(t)
	port := startServer(t, &TFTPServer{Root: root, UploadRoot: root, Whitelist: allowAll(t)})

	for _, name := range []string{"secret/file.txt", "secret/.goshs", "secret/.GOSHS", ".goshs", "blocked.txt", "BLOCKED.TXT"} {
		c := newClient(t, port)
		c.send(t, rrq(name))
		pkt := c.recv(t)
		require.Equal(t, uint16(opERROR), binary.BigEndian.Uint16(pkt[:2]), name)
		require.Equal(t, uint16(errFileNotFound), binary.BigEndian.Uint16(pkt[2:4]), name)
	}

	c := newClient(t, port)
	c.send(t, rrq("public.txt"))
	pkt := c.recv(t)
	require.Equal(t, uint16(opDATA), binary.BigEndian.Uint16(pkt[:2]), "unprotected file must stay readable")
	require.Equal(t, "public", string(pkt[4:]))
}

func TestWRQDeniedByACL(t *testing.T) {
	root := aclTree(t)
	port := startServer(t, &TFTPServer{Root: root, UploadRoot: root, Whitelist: allowAll(t)})

	for _, name := range []string{"secret/new.txt", "secret/.goshs", "sub/.goshs", "poison/.goshs/x"} {
		c := newClient(t, port)
		c.send(t, wrq(name))
		pkt := c.recv(t)
		require.Equal(t, uint16(opERROR), binary.BigEndian.Uint16(pkt[:2]), name)
		require.Equal(t, uint16(errAccessViolation), binary.BigEndian.Uint16(pkt[2:4]), name)
	}
	_, err := os.Stat(filepath.Join(root, "secret", "new.txt"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "poison"))
	require.True(t, os.IsNotExist(err), "no .goshs directory chain may be created")
	got, err := os.ReadFile(filepath.Join(root, "secret", ".goshs"))
	require.NoError(t, err)
	require.Contains(t, string(got), "admin:", "the ACL file must be untouched")
}
