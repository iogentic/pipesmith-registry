package catalog_test

import (
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

func TestComponentPathsGivesTheAssetsOfTheMatchingComponents(t *testing.T) {
	c := open(t, t.TempDir())
	specs := []testkit.BlobSpec{
		spec("demo-maven", "com/example/demo-lib/1.3.0/demo-lib-1.3.0.pom", "pom", t0),
		spec("demo-maven", "com/example/demo-lib/1.3.0/demo-lib-1.3.0.jar", "jar", t0),
		spec("demo-maven", "com/example/demo-lib/1.4.0/demo-lib-1.4.0.jar", "jar 4", t0),
		spec("demo-raw", "docs/a.txt", "a", t0),
	}
	for _, s := range specs {
		put(t, c, "write", s)
	}
	var asked [][3]string
	paths, err := c.ComponentPaths("demo-maven", func(namespace, name, version string) bool {
		asked = append(asked, [3]string{namespace, name, version})
		return version == "1.3.0"
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{specs[1].Path, specs[0].Path}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	if want := [][3]string{{"com.example", "demo-lib", "1.3.0"}, {"com.example", "demo-lib", "1.4.0"}}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("asked %v", asked)
	}
	if paths, err = c.ComponentPaths("demo-other", func(string, string, string) bool { return true }); err != nil || len(paths) != 0 {
		t.Fatalf("%v %v", paths, err)
	}
}
