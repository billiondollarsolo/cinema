package snapshot

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// racyWindow: a file modified within this long before it was hashed might be
// modified again without its stat changing (coarse mtime granularity), so its
// cache entry isn't trusted. Same idea as git's "racily clean" index entries.
const racyWindow = 2 * time.Second

// Cache maps (path, size, mtime, ctime, inode) → content digest so unchanged
// files aren't re-read (plan §5.4). Safe for concurrent use; methods accept nil.
type Cache struct {
	mu    sync.Mutex
	files map[string]cacheEntry
	seen  map[string]bool // paths touched by the latest Take; Save keeps only these
}

type cacheEntry struct {
	Stat     stat
	Digest   [32]byte
	Verified int64 // unix ns from before the content was read
}

// CachePath is where the cache for repo root lives inside the data dir.
func CachePath(dataDir, root string) string {
	h := sha256.Sum256([]byte(root))
	return filepath.Join(dataDir, "fscache", hex.EncodeToString(h[:8])+".gob")
}

// LoadCache reads a cache file; a missing or corrupt file yields an empty cache.
func LoadCache(path string) *Cache {
	c := &Cache{files: map[string]cacheEntry{}}
	if f, err := os.Open(path); err == nil {
		if gob.NewDecoder(f).Decode(&c.files) != nil {
			c.files = map[string]cacheEntry{}
		}
		f.Close()
	}
	return c
}

// Save writes entries seen by the latest Take (deleted files drop out) via temp + rename.
func (c *Cache) Save(path string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	keep := make(map[string]cacheEntry, len(c.seen))
	for p := range c.seen {
		if e, ok := c.files[p]; ok {
			keep[p] = e
		}
	}
	c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "*.tmp")
	if err != nil {
		return err
	}
	err = gob.NewEncoder(f).Encode(keep)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (c *Cache) lookup(path string, st stat) ([32]byte, bool) {
	if c == nil {
		return [32]byte{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	c.seen[path] = true
	e, ok := c.files[path]
	if !ok || e.Stat != st || st.Mtime >= e.Verified-int64(racyWindow) {
		return [32]byte{}, false
	}
	return e.Digest, true
}

// store records a digest; verified must be a time from before the file was read.
func (c *Cache) store(path string, st stat, d [32]byte, verified time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]cacheEntry{}
	}
	c.files[path] = cacheEntry{st, d, verified.UnixNano()}
}
