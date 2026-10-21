package task

import "testing"

func TestCacheDetector(t *testing.T) {
	for in, want := range map[string]bool{
		"ok  \tgithub.com/x/y\t0.12s\n":                     false,
		"ok  \tgithub.com/x/y\t(cached)\n":                  true,
		"ok  \tgithub.com/x/y\t(cached)\tcoverage: 80.0%\n": true,
		"=== RUN TestX\n--- PASS\nok  \tx\t(cached)":        true, // no trailing newline
		"log: the word (cached) appears\n":                  false,
		"FAIL\tx\t(cached)\n":                               false,
	} {
		d := &CacheDetector{}
		// Split writes at every byte boundary to exercise line reassembly.
		for i := range len(in) {
			d.Write([]byte{in[i]})
		}
		d.Done()
		if d.Cached != want {
			t.Errorf("%q: cached=%v, want %v", in, d.Cached, want)
		}
	}
}
