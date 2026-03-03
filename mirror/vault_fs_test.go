package mirror

import "testing"

func TestMemoryVaultFS_WriteAndRead(t *testing.T) {
	fs := NewMemoryVaultFS()
	if err := fs.WriteFile("NOTE/工作/test.md", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile("NOTE/工作/test.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", string(data), "hello")
	}
}

func TestMemoryVaultFS_Exists(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("a.md", []byte("x"))

	if !fs.Exists("a.md") {
		t.Error("should exist")
	}
	if fs.Exists("b.md") {
		t.Error("should not exist")
	}
}

func TestMemoryVaultFS_MkdirAll(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.MkdirAll("NOTE/工作/子目錄")

	if !fs.Exists("NOTE/工作/子目錄") {
		t.Error("directory should exist")
	}
	if !fs.Exists("NOTE/工作") {
		t.Error("parent directory should exist")
	}
}

func TestMemoryVaultFS_Remove(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("a.md", []byte("x"))
	fs.Remove("a.md")

	if fs.Exists("a.md") {
		t.Error("should not exist after remove")
	}
}

func TestMemoryVaultFS_RemoveAll(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("NOTE/工作/a.md", []byte("a"))
	fs.WriteFile("NOTE/工作/b.md", []byte("b"))
	fs.WriteFile("NOTE/生活/c.md", []byte("c"))

	fs.RemoveAll("NOTE/工作")

	if fs.Exists("NOTE/工作/a.md") {
		t.Error("a.md should be removed")
	}
	if fs.Exists("NOTE/工作/b.md") {
		t.Error("b.md should be removed")
	}
	if !fs.Exists("NOTE/生活/c.md") {
		t.Error("c.md should still exist")
	}
}

func TestMemoryVaultFS_Rename(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("old.md", []byte("content"))
	fs.Rename("old.md", "new.md")

	if fs.Exists("old.md") {
		t.Error("old path should not exist")
	}
	data, err := fs.ReadFile("new.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Errorf("got %q, want %q", string(data), "content")
	}
}

func TestMemoryVaultFS_ListDir(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("NOTE/a.md", []byte("a"))
	fs.WriteFile("NOTE/b.md", []byte("b"))
	fs.MkdirAll("NOTE/子目錄")
	fs.WriteFile("NOTE/子目錄/c.md", []byte("c"))

	entries, err := fs.ListDir("NOTE")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 3 {
		t.Errorf("got %d entries, want at least 3", len(entries))
	}
}

func TestMemoryVaultFS_Stat(t *testing.T) {
	fs := NewMemoryVaultFS()
	fs.WriteFile("test.md", []byte("hello world"))

	info, err := fs.Stat("test.md")
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Error("should not be a directory")
	}
	if info.Size() != 11 {
		t.Errorf("size: got %d, want 11", info.Size())
	}
}
