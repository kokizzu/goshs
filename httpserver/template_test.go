package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"goshs.de/goshs/v2/options"
)

// tplServer returns a FileServer with payload templating enabled, a given bound
// IP, and a parsed --tpl-var map, plus a temp webroot.
func tplServer(t *testing.T, ip string, vars map[string]string) (*FileServer, string) {
	t.Helper()
	dir := t.TempDir()
	fs, cleanup := newTestFileServer(t, dir)
	t.Cleanup(cleanup)
	fs.IP = ip
	fs.Options = &options.Options{Template: true, TemplateVarsParsed: vars}
	return fs, dir
}

func tplFile(t *testing.T, dir, name, content string) *os.File {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0644))
	f, err := os.Open(p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// ?tpl renders {{.LHOST}} (from the bound IP) and {{.LPORT}} (from --tpl-var).
func TestSendFile_Template_RendersVars(t *testing.T) {
	fs, dir := tplServer(t, "10.10.14.7", map[string]string{"LPORT": "4444"})
	f := tplFile(t, dir, "rev.ps1", "TCPClient('{{.LHOST}}','{{.LPORT}}')")

	r := httptest.NewRequest(http.MethodGet, "/rev.ps1?tpl", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "TCPClient('10.10.14.7','4444')", w.Body.String())
}

// A custom --tpl-var is exposed to the template.
func TestSendFile_Template_CustomVar(t *testing.T) {
	fs, dir := tplServer(t, "10.0.0.1", map[string]string{"FOO": "bar"})
	f := tplFile(t, dir, "x.txt", "value={{.FOO}}")

	r := httptest.NewRequest(http.MethodGet, "/x.txt?tpl", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "value=bar", w.Body.String())
}

// Without ?tpl, the file is served raw even when templating is enabled.
func TestSendFile_Template_NoParamServesRaw(t *testing.T) {
	fs, dir := tplServer(t, "10.0.0.1", nil)
	f := tplFile(t, dir, "x.txt", "raw {{.LHOST}}")

	r := httptest.NewRequest(http.MethodGet, "/x.txt", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "raw {{.LHOST}}", w.Body.String())
}

// With the feature disabled, ?tpl is ignored and the raw file is served.
func TestSendFile_Template_DisabledIgnoresParam(t *testing.T) {
	dir := t.TempDir()
	fs, cleanup := newTestFileServer(t, dir)
	defer cleanup()
	fs.Options = &options.Options{Template: false}
	f := tplFile(t, dir, "x.txt", "raw {{.LHOST}}")

	r := httptest.NewRequest(http.MethodGet, "/x.txt?tpl", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "raw {{.LHOST}}", w.Body.String())
}

// An unresolved {{.LHOST}} (bound to 0.0.0.0, no --tpl-var LHOST) errors loudly
// instead of emitting a blank.
func TestSendFile_Template_UnresolvedLHOST(t *testing.T) {
	fs, dir := tplServer(t, "0.0.0.0", nil)
	f := tplFile(t, dir, "rev.ps1", "connect {{.LHOST}}")

	r := httptest.NewRequest(http.MethodGet, "/rev.ps1?tpl", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "connect ", "must not emit a blank LHOST")
}

// Templating composes with ?download (attachment) ...
func TestSendFile_Template_Download(t *testing.T) {
	fs, dir := tplServer(t, "10.0.0.1", map[string]string{"LPORT": "9001"})
	f := tplFile(t, dir, "payload.sh", "nc {{.LHOST}} {{.LPORT}}")

	r := httptest.NewRequest(http.MethodGet, "/payload.sh?tpl&download", nil)
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "nc 10.0.0.1 9001", w.Body.String())
	require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
}

// ... and with HTTP Range over the rendered bytes.
func TestSendFile_Template_Range(t *testing.T) {
	fs, dir := tplServer(t, "10.0.0.1", nil)
	f := tplFile(t, dir, "x.txt", "HOST={{.Host}}") // -> "HOST=10.0.0.1" (12 bytes)

	r := httptest.NewRequest(http.MethodGet, "/x.txt?tpl", nil)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	fs.sendFile(w, r, f, configFile{})

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, "HOST", w.Body.String())
}
