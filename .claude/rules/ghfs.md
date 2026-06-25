<!-- ghfs:begin -->
# ghfs - GitHub Issues as Local Files

This project uses [ghfs](https://github.com/ghfs-proj/ghfs) to provide read-only
access to GitHub issues as local files.

## .ghfs/ Directory

The `.ghfs/` directory provides read-only access to GitHub issues as local files.
**Do not attempt to write to this directory.**

### Directory Structure

```
.ghfs/
└── {provider}/           # e.g., "github" or "github-work"
    └── issues/
        ├── open/
        │   └── {number}.md   # e.g., "42.md"
        └── closed/
            └── {number}.md
```

- `{provider}` is the provider name, optionally suffixed with the profile name
  (e.g., `github` for default profile, `github-work` for "work" profile)
- Issues are grouped by state (`open` / `closed`)
- Each issue is a Markdown file named by its number

### Issue File Format

Each issue file contains YAML frontmatter followed by the issue body in Markdown:

```markdown
---
number: 42
title: "Fix bug in authentication"
state: open
author: username
labels:
  - bug
  - priority/high
created_at: 2024-01-01T00:00:00Z
updated_at: 2024-01-15T10:30:00Z
---

Issue body in Markdown...

## Comments

### username - 2024-01-02T12:00:00Z

Comment content...
```

### Index File (.index.json)

`.index.json` is a hidden file that provides structured JSON data for all issues
including full content (body and comments). It is **not shown by** `ls` but can be
read by specifying the path directly.

Available index files:

| Path | Content |
|------|---------|
| `issues/.index.json` | All issues (open + closed) |
| `issues/open/.index.json` | Open issues only |
| `issues/closed/.index.json` | Closed issues only |

Use state-specific index files when you only need issues in a particular state,
as they are significantly smaller than the full index.

`.index.json` contains the **complete content** of every issue,
including the full body text and all comments. The data is identical to what is in
the individual `.md` files.

```json
[
  {
    "number": 42,
    "title": "Fix bug in authentication",
    "state": "open",
    "author": "username",
    "labels": ["bug", "priority/high"],
    "assignees": [],
    "url": "https://github.com/owner/repo/issues/42",
    "created_at": "2024-01-01T00:00:00Z",
    "updated_at": "2024-01-15T10:30:00Z",
    "body": "Issue body...",
    "comments": [
      {
        "author": "username",
        "body": "Comment content...",
        "created_at": "2024-01-02T12:00:00Z"
      }
    ]
  }
]
```

### How to Use

- **Read a specific issue:** `cat .ghfs/github/issues/open/{number}.md` — use this when investigating a specific issue
- **Browse open issues:** `ls .ghfs/github/issues/open/`
- **Cross-cutting investigation:** `cat .ghfs/github/issues/open/.index.json` — use `.index.json` only when investigating multiple issues at once
- **Search issues:** `grep -r "keyword" .ghfs/github/issues/`
- **Reference in code:** Use issue file paths to provide context about related issues

**Choosing the right approach — you MUST pick exactly one:**

- To investigate a **specific issue** (1-3 issues), read `{number}.md` directly. Do not read `.index.json` for single-issue lookups.
- To perform a **cross-cutting investigation** across multiple issues (4+), read `.index.json` once. `.index.json` already contains the complete body and comments for every issue — there is **no need** to read individual `.md` files afterward. Reading both `.index.json` and individual `.md` files is redundant and wastes tokens.

**NEVER do both.** Either use `.index.json` alone OR individual `.md` files alone — never combine them in the same task.

### Important Notes

- The directory is **read-only** — do not attempt to create, modify, or delete files
- Files are automatically updated from GitHub
- `.ghfs/` is listed in `.gitignore` and should not be committed
- If `.ghfs/` is empty or inaccessible, ghfs may not be running

<!-- ghfs:end -->
