package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRecipeFilesListsOnlyLocalSources(t *testing.T) {
	dir := t.TempDir()
	pkgbuild := `pkgname=x
source=(https://example.com/x-${pkgname}.tar.gz
  config
  logo.bin::logo.bin
  "m1n1.tar.gz::https://github.com/a/b/archive/c.tar.gz"
  git+https://github.com/a/b.git)
source_aarch64=(arm.patch)
install=x.install
`
	for name, body := range map[string]string{"PKGBUILD": pkgbuild, "config": "", "logo.bin": "", "arm.patch": "", "x.install": "", "big-download.tar.gz": ""} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	got, err := recipeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PKGBUILD", "config", "logo.bin", "arm.patch", "x.install"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	os.Remove(filepath.Join(dir, "config"))
	if _, err := recipeFiles(dir); err == nil {
		t.Fatal("a missing local source should be an error")
	}
}
