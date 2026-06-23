package httpserver

import (
	"context"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"

	"golang.org/x/net/webdav"
)

// webdavCtxKey carries the originating *http.Request into the webdav
// FileSystem so per-path .goshs ACLs can be evaluated against the caller's
// credentials (the FileSystem interface only receives a context.Context).
type webdavCtxKey struct{}

func reqFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(webdavCtxKey{}).(*http.Request)
	return r
}

// newWebdavFileSystem returns a webdav.FileSystem rooted at the webroot that
// enforces the same .goshs ACL rules as the HTTP file server.
func (fs *FileServer) newWebdavFileSystem() webdav.FileSystem {
	return webdavACLFileSystem{srv: fs, root: webdav.Dir(fs.Webroot)}
}

// webdavEnforceACL applies the .goshs ACL to the directly-addressed WebDAV
// resource, mirroring the HTTP server (sendFile/doDir). It is the choke point
// in wdGuard and, unlike the FileSystem layer below, can return a proper 401
// with a WWW-Authenticate challenge so WebDAV clients can supply per-directory
// credentials. It returns false (after writing a response) when the request
// must not proceed.
func (fs *FileServer) webdavEnforceACL(w http.ResponseWriter, r *http.Request) bool {
	abs, err := sanitizePath(fs.Webroot, r.URL.Path)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return false
	}
	// Never expose the ACL config file itself — it holds the bcrypt hashes.
	if filepath.Base(abs) == ".goshs" {
		http.NotFound(w, r)
		return false
	}
	// A directory is governed by its own .goshs; a file by its parent's.
	governing := abs
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		governing = filepath.Dir(abs)
	}
	acl, _ := fs.findEffectiveACL(governing)
	if !fs.applyCustomAuth(w, r, acl) {
		return false
	}
	if slices.Contains(acl.Block, filepath.Base(abs)) {
		http.NotFound(w, r)
		return false
	}
	return true
}

// webdavACLFileSystem wraps a webdav.FileSystem and enforces the .goshs ACL on
// every operation, including the recursive directory walks PROPFIND performs.
// This prevents listings from revealing the .goshs file, block-listed entries,
// or password-protected subdirectories the caller is not authorised for.
type webdavACLFileSystem struct {
	srv  *FileServer
	root webdav.FileSystem
}

// aclError mirrors the HTTP server's behaviour for ACL violations:
//   - the .goshs file and block-listed names look like they do not exist (404)
//   - an unsatisfied auth requirement is a permission error (403)
//
// It returns nil when access to name is permitted.
func (a webdavACLFileSystem) aclError(ctx context.Context, name string) error {
	if path.Base(name) == ".goshs" {
		return os.ErrNotExist
	}
	abs, err := sanitizePath(a.srv.Webroot, name)
	if err != nil {
		return os.ErrNotExist
	}
	governing := abs
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		governing = filepath.Dir(abs)
	}
	acl, _ := a.srv.findEffectiveACL(governing)
	if acl.Auth != "" && !aclSatisfied(reqFromContext(ctx), acl) {
		return os.ErrPermission
	}
	if slices.Contains(acl.Block, path.Base(name)) {
		return os.ErrNotExist
	}
	return nil
}

func (a webdavACLFileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if err := a.aclError(ctx, name); err != nil {
		return nil, err
	}
	f, err := a.root.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return nil, err
	}
	abs, _ := sanitizePath(a.srv.Webroot, name)
	return aclFile{File: f, srv: a.srv, req: reqFromContext(ctx), dir: abs}, nil
}

func (a webdavACLFileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if err := a.aclError(ctx, name); err != nil {
		return nil, err
	}
	return a.root.Stat(ctx, name)
}

func (a webdavACLFileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if err := a.aclError(ctx, name); err != nil {
		return err
	}
	return a.root.Mkdir(ctx, name, perm)
}

func (a webdavACLFileSystem) RemoveAll(ctx context.Context, name string) error {
	if err := a.aclError(ctx, name); err != nil {
		return err
	}
	return a.root.RemoveAll(ctx, name)
}

func (a webdavACLFileSystem) Rename(ctx context.Context, oldName, newName string) error {
	if err := a.aclError(ctx, oldName); err != nil {
		return err
	}
	if err := a.aclError(ctx, newName); err != nil {
		return err
	}
	return a.root.Rename(ctx, oldName, newName)
}

// aclFile wraps a webdav.File so directory listings (PROPFIND) cannot reveal the
// .goshs config, block-listed entries, or password-protected subdirectories the
// caller is not authorised for.
type aclFile struct {
	webdav.File
	srv *FileServer
	req *http.Request
	dir string // absolute path of this resource
}

func (f aclFile) Readdir(count int) ([]os.FileInfo, error) {
	infos, err := f.File.Readdir(count)
	if err != nil {
		return infos, err
	}
	// Access to f.dir itself was already authorised; here we filter what is
	// visible inside it. acl is the effective ACL governing this directory.
	acl, _ := f.srv.findEffectiveACL(f.dir)
	filtered := infos[:0]
	for _, fi := range infos {
		name := fi.Name()
		if name == ".goshs" {
			continue
		}
		if slices.Contains(acl.Block, name) {
			continue
		}
		// A subdirectory may add its own auth requirement; hide it if unmet.
		if fi.IsDir() {
			childACL, _ := f.srv.findEffectiveACL(filepath.Join(f.dir, name))
			if childACL.Auth != "" && !aclSatisfied(f.req, childACL) {
				continue
			}
		}
		filtered = append(filtered, fi)
	}
	return filtered, nil
}
