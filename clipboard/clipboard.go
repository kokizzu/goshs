// Package clipboard will provide the functionality of a clipboard
package clipboard

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Clipboard is the in memory clipboard to hold the copy-pasteable content
type Clipboard struct {
	mu      sync.RWMutex
	Entries []Entry
}

// Entry will represent a single entry in the clipboard
type Entry struct {
	ID      int
	Content string
	Time    string
}

// New will return an instantiated Clipboard
func New() *Clipboard {
	return &Clipboard{}
}

// AddEntry will give the opportunity to add an entry to the clipboard
func (c *Clipboard) AddEntry(con string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.Entries
	id := 0
	if len(entries) > 0 {
		id = len(entries)
	}
	c.Entries = append([]Entry{{
		ID:      id,
		Content: con,
		Time:    time.Now().Format("Mon Jan _2 15:04:05 2006"),
	}}, entries...)
	return nil
}

// DeleteEntry will give the opportunity to delete an entry from the clipboard
func (c *Clipboard) DeleteEntry(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.Entries
	if id < 0 {
		return fmt.Errorf("id cannot be negative: %d", id)
	}
	if id >= len(entries) {
		return fmt.Errorf("invalid entry ID: %d", id)
	}
	entries = append(entries[:id], entries[id+1:]...)
	c.Entries = reindex(entries)
	return nil
}

// ClearClipboard will empty the clipboard
func (c *Clipboard) ClearClipboard() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Entries = nil
	return nil
}

// GetEntries will give the opportunity to receive the entries from the clipboard
func (c *Clipboard) GetEntries() ([]Entry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, len(c.Entries))
	copy(out, c.Entries)
	return out, nil
}

// Download will return a json encoded representation of the clipboards content for download purposes
func (c *Clipboard) Download() ([]byte, error) {
	c.mu.RLock()
	entries := make([]Entry, len(c.Entries))
	copy(entries, c.Entries)
	c.mu.RUnlock()

	return json.MarshalIndent(entries, "", "    ")
}

func reindex(entries []Entry) []Entry {
	n := len(entries)
	for i := range entries {
		entries[i].ID = n - 1 - i
	}
	return entries
}
