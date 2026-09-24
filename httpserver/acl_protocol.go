package httpserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ProtocolACL enforces the per-directory .goshs ACL for file-transfer protocols
// other than HTTP/WebDAV — SFTP, FTP, TFTP and SMB — which serve the same
// webroot but carry no per-request HTTP credential and therefore cannot satisfy
// a folder's basic-auth requirement.
//
// It fails closed, mirroring webdavEnforceACL / aclFile.Readdir: the .goshs file
// itself is never accessible; a path governed by a .goshs Auth requirement is
// denied outright (there is no way to present the per-folder credential over
// these protocols, so an unsatisfiable requirement means deny); and any path
// whose basename is on the effective block list is denied. This closes
// GHSA-2m7f-jq4x-rcj7, where the SFTP request handlers performed only a
// webroot-boundary check and never consulted the .goshs ACL at all, and
// GHSA-q8gg-q2wc-w52g, the same gap in TFTP, FTP and SMB.
type ProtocolACL struct {
	fs *FileServer
}

// blockListed reports whether name appears in the block list using
// case-insensitive comparison, so that alternate-case spellings (e.g.
// SECRET.TXT for an on-disk secret.txt) are caught on case-insensitive
// filesystems (Windows NTFS, macOS APFS/HFS+) — GHSA-3x28-6v7h-gg87.
func blockListed(block []string, name string) bool {
	return slices.ContainsFunc(block, func(s string) bool {
		return strings.EqualFold(s, name)
	})
}

// NewProtocolACL builds a ProtocolACL bound to webroot. Only the webroot is
// needed: findEffectiveACL/findSpecialFile read it plus the .goshs files on disk
// and touch no other FileServer state.
func NewProtocolACL(webroot string) *ProtocolACL {
	return &ProtocolACL{fs: &FileServer{Webroot: webroot}}
}

// Allowed reports whether an operation on absPath (an absolute path inside the
// webroot) is permitted. absPath need not exist yet (e.g. an upload target or a
// mkdir): as in webdavEnforceACL, a missing or non-directory path is governed by
// its parent directory's effective ACL, while an existing directory is governed
// by its own.
func (p *ProtocolACL) Allowed(absPath string) bool {
	// Never expose the ACL config file itself — it holds the bcrypt hashes —
	// and never touch a path beneath a .goshs-named directory, which could only
	// exist to mask an ACL file (GHSA-mhxc-hfx2-7w79).
	if strings.EqualFold(filepath.Base(absPath), ".goshs") {
		return false
	}
	if rel, err := filepath.Rel(p.fs.Webroot, absPath); err == nil && containsACLName(rel) {
		return false
	}
	governing := absPath
	if info, err := os.Stat(absPath); err != nil || !info.IsDir() {
		governing = filepath.Dir(absPath)
	}
	acl, _ := p.fs.findEffectiveACL(governing)
	// A per-folder auth requirement cannot be satisfied over a credential-less
	// protocol, so it is a hard deny (the HTTP/WebDAV equivalent of an
	// unsatisfied aclSatisfied check).
	if acl.Auth != "" {
		return false
	}
	if blockListed(acl.Block, filepath.Base(absPath)) {
		return false
	}
	return true
}

// FilterListing removes entries a protocol client must not see from a listing of
// dir (an absolute path already authorised via Allowed): the .goshs file itself,
// block-listed names, and subdirectories that carry their own auth requirement.
// Mirrors aclFile.Readdir.
func (p *ProtocolACL) FilterListing(dir string, infos []os.FileInfo) []os.FileInfo {
	acl, _ := p.fs.findEffectiveACL(dir)
	filtered := infos[:0]
	for _, fi := range infos {
		if p.visible(dir, acl, fi.Name(), fi.IsDir()) {
			filtered = append(filtered, fi)
		}
	}
	return filtered
}

// FilterDirEntries is FilterListing for os.ReadDir results.
func (p *ProtocolACL) FilterDirEntries(dir string, entries []os.DirEntry) []os.DirEntry {
	acl, _ := p.fs.findEffectiveACL(dir)
	filtered := entries[:0]
	for _, de := range entries {
		if p.visible(dir, acl, de.Name(), de.IsDir()) {
			filtered = append(filtered, de)
		}
	}
	return filtered
}

// visible reports whether entry name of dir (whose effective ACL is acl) may be
// shown to a credential-less protocol client.
func (p *ProtocolACL) visible(dir string, acl configFile, name string, isDir bool) bool {
	if strings.EqualFold(name, ".goshs") {
		return false
	}
	if blockListed(acl.Block, name) {
		return false
	}
	if isDir {
		childACL, _ := p.fs.findEffectiveACL(filepath.Join(dir, name))
		if childACL.Auth != "" {
			return false
		}
	}
	return true
}
