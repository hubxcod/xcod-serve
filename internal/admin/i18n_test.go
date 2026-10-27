package admin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"xcod/internal/config"
	"xcod/internal/i18n"
)

// Every panel string must have a translation in each language file. The strings are found
// where they are used: templates ({{t}}/{{tf}}), JavaScript (t()), and the Go code that sends
// messages to the panel (i18n.Errorf, s.redirect, s.tr, page titles).
func TestTranslationsComplete(t *testing.T) {
	keys := map[string]string{} // key -> where it was found
	add := func(raw, where string) {
		s, err := strconv.Unquote(raw)
		if err != nil {
			t.Fatalf("%s: cannot read %s: %v", where, raw, err)
		}
		if s != "" {
			keys[s] = where
		}
	}
	const str = `("(?:[^"\\]|\\.)*")`
	scan := func(root string, exts []string, patterns []*regexp.Regexp) {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			ok := false
			for _, e := range exts {
				ok = ok || strings.HasSuffix(path, e)
			}
			if !ok {
				return nil
			}
			b, _ := os.ReadFile(path)
			for _, re := range patterns {
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					add(m[1], path)
				}
			}
			return nil
		})
	}
	scan("views", []string{".html"}, []*regexp.Regexp{
		regexp.MustCompile(`\{\{\s*tf?\s+` + str),
	})
	scan("static", []string{".js"}, []*regexp.Regexp{
		regexp.MustCompile(`\bt\(` + str),
	})
	scan("..", []string{".go"}, []*regexp.Regexp{
		regexp.MustCompile(`i18n\.Errorf\(` + str),
		regexp.MustCompile(`s\.redirect\(w, r, "[^"]*", "(?:ok|err)", ` + str),
		regexp.MustCompile(`s\.tr\(r, ` + str),
		regexp.MustCompile(`s\.render\(w, r, [^,]+, "[a-z_]+", ` + str),
		regexp.MustCompile(`i18n\.(?:T|Sprintf)\([a-z]+, ` + str),
		regexp.MustCompile(`\bmsg :?= ` + str),
		regexp.MustCompile(`\bset\(` + str),
	})
	for _, q := range config.Qualities {
		keys[q.Label] = "config.Qualities"
	}

	for _, l := range i18n.Langs {
		if l.Code == i18n.Default {
			continue
		}
		cat := i18n.Catalog(l.Code)
		var missing []string
		for k, where := range keys {
			if cat[k] == "" {
				missing = append(missing, strconv.Quote(k)+"  ("+where+")")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: %d string(s) without translation:\n%s", l.Code, len(missing), strings.Join(missing, "\n"))
		}
		for k, v := range cat { // the verbs must match, or the message breaks
			if verbs(k) != verbs(v) {
				t.Errorf("%s: %q -> %q: format verbs differ", l.Code, k, v)
			}
		}
	}
}

// verbs are fmt verbs (%s, %d, %q…) and {name} placeholders; a plain "80%" is not one.
var verbRe = regexp.MustCompile(`%[0-9.]*[sdvqfx]|\{[a-z]+\}`)

func verbs(s string) string {
	v := verbRe.FindAllString(s, -1)
	sort.Strings(v)
	return strings.Join(v, " ")
}
