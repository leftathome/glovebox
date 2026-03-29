package connector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Checkpoint interface {
	Load(key string) (string, bool)
	Save(key string, value string)
	Delete(key string)
}

type fileCheckpoint struct {
	path string
	mu   sync.RWMutex
	data map[string]string
}

func NewCheckpoint(stateDir string) (Checkpoint, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	path := filepath.Join(stateDir, "state.json")
	cp := &fileCheckpoint{
		path: path,
		data: make(map[string]string),
	}

	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &cp.data); err != nil {
			return nil, fmt.Errorf("parse state file: %w", err)
		}
	}

	return cp, nil
}

func (c *fileCheckpoint) Load(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *fileCheckpoint) Save(key string, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = value
	c.persist()
}

func (c *fileCheckpoint) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	c.persist()
}

func (c *fileCheckpoint) persist() {
	data, err := json.Marshal(c.data)
	if err != nil {
		return
	}

	tmpPath := c.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return
	}
	os.Rename(tmpPath, c.path)
}
