package uniparse

import "testing"

func TestUnmarshalYAMLIntoStruct(t *testing.T) {
	var pkg struct {
		Package string `json:"package" yaml:"package"`
		Version string `json:"version" yaml:"version"`
	}
	data := []byte("package: zlib\nversion: 1.3.1\n")
	if err := Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Package != "zlib" || pkg.Version != "1.3.1" {
		t.Fatalf("got %+v", pkg)
	}
}

func TestUnmarshalJSONIntoStruct(t *testing.T) {
	var pkg struct {
		Package string `json:"package" yaml:"package"`
	}
	if err := Unmarshal([]byte(`{"package":"zlib"}`), &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Package != "zlib" {
		t.Fatalf("got %+v", pkg)
	}
}
