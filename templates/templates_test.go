package templates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedCatalog(t *testing.T) {
	c, err := load(files, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != len(manifest) {
		t.Fatal("manifest differs from embedded files")
	}
	data := Data{}
	value := reflect.ValueOf(&data).Elem()
	for i := 0; i < value.NumField(); i++ {
		value.Field(i).SetString("bound-value")
	}
	for id := range manifest {
		if strings.TrimSpace(c.render(id, data)) == "" {
			t.Errorf("template %s renders empty copy", id)
		}
	}
}

func TestInvalidEditsFailAtLoad(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "whitespace": " \n\t\n", "invalid UTF8": "\xff", "NUL": "text\x00",
		"malformed": "{{.Amount", "unknown field": "{{.Unknown}}", "unavailable field": "{{.SubscriptionURL}}",
		"nested field": "{{.Amount.String}}", "method": "{{.String}}", "function": "{{print .Amount}}",
		"pipeline": "{{.Amount | printf \"%s\"}}", "if": "{{if .Amount}}yes{{end}}", "range": "{{range .Amount}}yes{{end}}",
		"with": "{{with .Amount}}yes{{end}}", "variable": "{{$a := .Amount}}{{$a}}", "root": "{{.}}", "root variable": "{{$.Amount}}",
		"literal action": "{{\"copy\"}}", "comment": "a{{/* hidden */}}b", "template": "{{template \"other\" .}}",
		"definition": "{{define \"other\"}}text{{end}}text", "empty definition": "{{define \"other\"}}{{end}}text",
		"self definition": `{{define "group/message"}}text{{end}}`,
		"trim action":     "x {{- .Amount}}",
		"block":           "{{block \"other\" .}}text{{end}}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(fstest.MapFS{"group/message.tmpl": {Data: []byte(body)}}, map[string][]string{"group/message": {"Amount"}})
			if err == nil {
				t.Fatal("invalid edit accepted")
			}
		})
	}
	for name, source := range map[string]fstest.MapFS{
		"missing":              {},
		"unknown":              {"group/message.tmpl": {Data: []byte("copy")}, "group/other.tmpl": {Data: []byte("copy")}},
		"unexpected extension": {"group/message.tmpl": {Data: []byte("copy")}, "group/note.txt": {Data: []byte("note")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(source, map[string][]string{"group/message": {}}); err == nil {
				t.Fatal("missing or unknown file accepted")
			}
		})
	}
}

func TestSubstitutionIsLiteralAndWhitespaceIsPreserved(t *testing.T) {
	c, err := load(fstest.MapFS{"group/message.tmpl": {Data: []byte(" \n<{{.DisplayName}}> & {{.Amount}} / {{.Amount}}\n\n\n")}}, map[string][]string{"group/message": {"DisplayName", "Amount"}})
	if err != nil {
		t.Fatal(err)
	}
	got := c.render("group/message", Data{DisplayName: "{{.SubscriptionURL}}", Amount: "9007199254740993.010000000000000001", SubscriptionURL: "synthetic-secret"})
	want := " \n<{{.SubscriptionURL}}> & 9007199254740993.010000000000000001 / 9007199254740993.010000000000000001\n\n"
	if got != want {
		t.Fatalf("literal substitution or whitespace changed: %q", got)
	}
}

func TestLabelsAreDistinctAndNonempty(t *testing.T) {
	for _, edit := range []string{" \n", "same\n", "/status\n"} {
		source := fstest.MapFS{}
		known := map[string][]string{}
		for _, label := range []string{"invoice", "status", "configuration", "help"} {
			id := "clientmenu/label_" + label
			source[id+".tmpl"] = &fstest.MapFile{Data: []byte(edit)}
			known[id] = nil
		}
		if _, err := load(source, known); err == nil {
			t.Fatal("empty/ambiguous labels accepted")
		}
	}
}

func TestPersonalURLIsOnlyAvailableToProtectedTemplate(t *testing.T) {
	const secret = "synthetic-secret-subscription"
	if !strings.Contains(Render("configdelivery/subscription", Data{SubscriptionURL: secret}), secret) {
		t.Fatal("protected template omitted its personal URL")
	}
	for id, allowed := range manifest {
		if id == "configdelivery/subscription" {
			continue
		}
		for _, field := range allowed {
			if field == "SubscriptionURL" {
				t.Fatalf("personal URL available in ordinary template %s", id)
			}
		}
		if strings.Contains(Render(id, Data{SubscriptionURL: "synthetic-secret-subscription"}), "synthetic-secret-subscription") {
			t.Fatalf("personal URL leaked by %s", id)
		}
	}
}

// Catch misspelled literal IDs and fields at every production call site, even
// rarely reached review branches. Domain objects cannot cross this boundary.
func TestRenderCallSites(t *testing.T) {
	used := map[string]bool{}
	err := filepath.WalkDir("../internal", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Render" {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if !ok || owner.Name != "templates" {
				return true
			}
			if len(call.Args) != 2 {
				t.Fatalf("unexpected Render arguments: %s", path)
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("nonliteral template ID: %s", path)
			}
			id, _ := strconv.Unquote(literal.Value)
			allowed, ok := manifest[id]
			if !ok {
				t.Fatalf("unknown template ID %s: %s", id, path)
			}
			used[id] = true
			data, ok := call.Args[1].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("Render requires explicit display fields: %s", path)
			}
			bound := map[string]bool{}
			for _, e := range data.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					t.Fatalf("unkeyed display data: %s", path)
				}
				key := kv.Key.(*ast.Ident).Name
				bound[key] = true
				found := false
				for _, field := range allowed {
					found = found || key == field
				}
				if !found {
					t.Errorf("unexpected binding %s in %s: %s", key, id, path)
				}
			}
			for _, field := range allowed {
				if !bound[field] {
					t.Errorf("missing binding %s in %s: %s", field, id, path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id := range manifest {
		if !used[id] {
			t.Errorf("unused template: %s", id)
		}
	}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[Data]()) {
		if f.Type.Kind() != reflect.String {
			t.Fatalf("non-string display field: %s", f.Name)
		}
	}
}

func TestUnknownIDIsProgrammerError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unknown ID did not fail")
		}
	}()
	Render("not/a/template", Data{})
}
