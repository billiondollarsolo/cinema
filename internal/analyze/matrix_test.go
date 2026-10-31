package analyze

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func legs(t *testing.T, src string) []string {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	combos, err := expand(n.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range combos {
		var kv []string
		for _, k := range sortedKeys(c) {
			kv = append(kv, k+"="+c[k])
		}
		out = append(out, strings.Join(kv, ","))
	}
	return out
}

// GitHub's documented include example, and click's include-only matrix where
// two entries share a value (python 3.14 on Linux and on a Mac): every entry
// is its own leg, none extends a leg another entry added.
func TestExpandIncludeLikeActions(t *testing.T) {
	got := legs(t, `
fruit: [apple, pear]
animal: [cat, dog]
include:
  - color: green
  - {color: pink, animal: cat}
  - {fruit: apple, shape: circle}
  - fruit: banana
  - {fruit: banana, animal: cat}
`)
	want := []string{
		"animal=cat,color=pink,fruit=apple,shape=circle",
		"animal=dog,color=green,fruit=apple,shape=circle",
		"animal=cat,color=pink,fruit=pear",
		"animal=dog,color=green,fruit=pear",
		"fruit=banana",
		"animal=cat,fruit=banana",
	}
	if !slices.Equal(got, want) {
		t.Errorf("docs example:\n got %q\nwant %q", got, want)
	}
	got = legs(t, "include: [{python: '3.14'}, {name: Mac, python: '3.14', os: macos-latest}]")
	if want := []string{"python=3.14", "name=Mac,os=macos-latest,python=3.14"}; !slices.Equal(got, want) {
		t.Errorf("include only: got %q, want %q", got, want)
	}
}
