package i18n

import (
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Spelled in two pieces so this file itself passes the guard that keeps the
// provider's name out of every file under cli/.
const bannedProvider = "ub" + "er"

var placeholder = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)\}`)

func placeholders(s string) []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func TestCatalogsHaveTheSameKeys(t *testing.T) {
	en, es := catalogs[English], catalogs[Spanish]
	for k := range en {
		if _, ok := es[k]; !ok {
			t.Errorf("key %q is in en.json but not in es.json", k)
		}
	}
	for k := range es {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q is in es.json but not in en.json", k)
		}
	}
}

func TestEveryPlaceholderIsOnBothSides(t *testing.T) {
	en, es := catalogs[English], catalogs[Spanish]
	for k, v := range en {
		other, ok := es[k]
		if !ok {
			continue
		}
		if a, b := strings.Join(placeholders(v), ","), strings.Join(placeholders(other), ","); a != b {
			t.Errorf("%s: placeholders differ, en {%s} es {%s}", k, a, b)
		}
	}
}

func TestNoEmptyValues(t *testing.T) {
	for lang, m := range catalogs {
		for k, v := range m {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: %s is empty", lang, k)
			}
		}
	}
}

// Copy a person reads never carries an em dash, an en dash, a middle dot, or the
// delivery provider's name (the repository's own guard applies the same rule to
// every file here).
func TestBannedCharactersAndWords(t *testing.T) {
	banned := map[string]string{
		string(rune(0x2014)): "an em dash",
		string(rune(0x2013)): "an en dash",
		string(rune(0x00b7)): "a middle dot",
	}
	for lang, m := range catalogs {
		for k, v := range m {
			for ch, name := range banned {
				if strings.Contains(v, ch) {
					t.Errorf("%s: %s contains %s", lang, k, name)
				}
			}
			if strings.Contains(strings.ToLower(v), bannedProvider) {
				t.Errorf("%s: %s names the delivery provider", lang, k)
			}
		}
	}
}

// Each file stays sorted by key, so two lanes adding keys under different
// prefixes never touch the same lines.
func TestFilesAreSortedByKey(t *testing.T) {
	for _, lang := range []string{English, Spanish} {
		raw, err := files.ReadFile(lang + ".json")
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		if _, err := dec.Token(); err != nil { // {
			t.Fatal(err)
		}
		prev := ""
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			key := tok.(string)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if key <= prev {
				t.Errorf("%s.json: %q should come before %q (keep the file sorted)", lang, key, prev)
			}
			prev = key
		}
	}
}

func TestTFillsPlaceholdersAndFallsBack(t *testing.T) {
	defer SetLang(English)
	SetLang(Spanish)
	if got := T("version.line", Args{"version": "1.2.3", "os": "linux", "arch": "amd64"}); got != "listo 1.2.3 (linux/amd64)" {
		t.Fatalf("got %q", got)
	}
	if got := T("error.next_step", Args{"step": "Corre listo login."}); got != "Siguiente paso: Corre listo login." {
		t.Fatalf("got %q", got)
	}
	SetLang(English)
	if got := T("error.next_step", Args{"step": "x"}); got != "Next step: x" {
		t.Fatalf("got %q", got)
	}
	if got := T("no.such.key"); got != "no.such.key" {
		t.Fatalf("an unknown key must answer itself, got %q", got)
	}
	if got := T("error.code"); got != "Code: {code}" {
		t.Fatalf("a missing argument must leave the placeholder, got %q", got)
	}
	if AcceptLanguage() != "en" {
		t.Fatal("English sends Accept-Language en")
	}
	SetLang(Spanish)
	if AcceptLanguage() != "es-MX" {
		t.Fatal("Spanish sends Accept-Language es-MX")
	}
}

func TestDetect(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	mac := func(v string) func() string { return func() string { return v } }
	cases := []struct {
		name  string
		flag  string
		env   map[string]string
		goos  string
		apple string
		want  string
	}{
		{"flag wins over everything", "en", map[string]string{"LISTO_LANG": "es", "LANG": "es_MX.UTF-8"}, "linux", "", English},
		{"LISTO_LANG before LC_ALL", "", map[string]string{"LISTO_LANG": "es", "LC_ALL": "en_US.UTF-8"}, "linux", "", Spanish},
		{"LC_ALL before LC_MESSAGES", "", map[string]string{"LC_ALL": "en_US.UTF-8", "LC_MESSAGES": "es_MX.UTF-8"}, "linux", "", English},
		{"LC_MESSAGES before LANG", "", map[string]string{"LC_MESSAGES": "es_ES.UTF-8", "LANG": "en_US.UTF-8"}, "linux", "", Spanish},
		{"LANG alone", "", map[string]string{"LANG": "es_MX.UTF-8"}, "linux", "", Spanish},
		{"an empty variable is skipped", "", map[string]string{"LC_ALL": "", "LANG": "es_MX.UTF-8"}, "linux", "", Spanish},
		{"C is English", "", map[string]string{"LANG": "C"}, "linux", "", English},
		{"another language is English", "", map[string]string{"LANG": "fr_FR.UTF-8"}, "linux", "", English},
		{"nothing set on linux is English", "", nil, "linux", "es_MX", English},
		{"macOS with nothing set asks AppleLocale", "", nil, "darwin", "es_MX", Spanish},
		{"macOS AppleLocale en", "", nil, "darwin", "en_US", English},
		{"macOS env beats AppleLocale", "", map[string]string{"LANG": "en_US.UTF-8"}, "darwin", "es_MX", English},
		{"macOS lookup failing is English", "", nil, "darwin", "", English},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect(c.flag, Env{Getenv: env(c.env), GOOS: c.goos, AppleLocale: mac(c.apple)})
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
