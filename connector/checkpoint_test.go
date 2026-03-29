package connector

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestCheckpoint_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	cp, err := NewCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Save("key1", "value1"); err != nil {
		t.Fatal(err)
	}
	v, ok := cp.Load("key1")
	if !ok || v != "value1" {
		t.Errorf("Load = (%q, %v), want (value1, true)", v, ok)
	}
}

func TestCheckpoint_LoadMissing(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewCheckpoint(dir)
	_, ok := cp.Load("nonexistent")
	if ok {
		t.Error("Load should return false for missing key")
	}
}

func TestCheckpoint_Delete(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewCheckpoint(dir)
	cp.Save("key1", "value1")
	if err := cp.Delete("key1"); err != nil {
		t.Fatal(err)
	}
	_, ok := cp.Load("key1")
	if ok {
		t.Error("key should be deleted")
	}
}

func TestCheckpoint_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cp1, _ := NewCheckpoint(dir)
	cp1.Save("persistent", "data")

	cp2, err := NewCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := cp2.Load("persistent")
	if !ok || v != "data" {
		t.Errorf("after reopen: Load = (%q, %v), want (data, true)", v, ok)
	}
}

func TestCheckpoint_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewCheckpoint(dir)

	cp.Save("key1", "value1")
	cp.Save("key2", "value2")

	cp2, _ := NewCheckpoint(dir)
	v1, _ := cp2.Load("key1")
	v2, _ := cp2.Load("key2")
	if v1 != "value1" || v2 != "value2" {
		t.Errorf("state incomplete after reopen: key1=%q key2=%q", v1, v2)
	}
}

func TestCheckpoint_ConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewCheckpoint(dir)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cp.Save(filepath.Base(t.Name()), "value")
		}(i)
	}
	wg.Wait()
}

func TestCheckpoint_EmptyStateDir(t *testing.T) {
	dir := t.TempDir()
	cp, err := NewCheckpoint(filepath.Join(dir, "new-subdir"))
	if err != nil {
		t.Fatal(err)
	}
	_, ok := cp.Load("anything")
	if ok {
		t.Error("empty checkpoint should have no keys")
	}
}

func TestCheckpoint_SaveReturnsError(t *testing.T) {
	// Save to a valid dir should succeed
	dir := t.TempDir()
	cp, _ := NewCheckpoint(dir)
	err := cp.Save("key", "value")
	if err != nil {
		t.Errorf("Save should succeed: %v", err)
	}
}
