// Package templates loads and validates the bot's embedded plain-text copy at
// initialization, before any application effects. It has no runtime overrides.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode/utf8"
)

//go:embed */*
var files embed.FS

// A part is either literal text or a validated index of a string field in Data.
// Compiling the restricted AST avoids request-time template execution errors.
type part struct {
	text  string
	field int // -1 for literal text
}

type catalog map[string][]part

var fieldAction = regexp.MustCompile(`{{\s*\.[A-Za-z][A-Za-z0-9_]*\s*}}`)

var loaded = mustLoad()

func mustLoad() catalog {
	c, err := load(files, manifest)
	if err != nil {
		panic(err)
	}
	return c
}

func load(source fs.FS, known map[string][]string) (catalog, error) {
	c := catalog{}
	err := fs.WalkDir(source, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		id := strings.TrimSuffix(path, ".tmpl")
		allowed, ok := known[id]
		if !ok || path != id+".tmpl" {
			return fmt.Errorf("unknown message template: %s", path)
		}
		b, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		// Remove exactly one editor newline, preserving all intentional whitespace.
		s := strings.TrimSuffix(string(b), "\n")
		if !utf8.ValidString(s) || strings.ContainsRune(s, '\x00') || strings.TrimSpace(s) == "" {
			return fmt.Errorf("empty or invalid message template: %s", path)
		}
		t, err := template.New(id).Parse(s)
		if err != nil {
			return fmt.Errorf("message template %s: %w", path, err)
		}
		if len(t.Templates()) != 1 {
			return fmt.Errorf("definitions forbidden in message template: %s", path)
		}
		// Definitions and comments can disappear from the parsed root (including
		// a definition with the root's own name). Reject every source action
		// except a simple field before compiling the AST.
		if strings.Contains(fieldAction.ReplaceAllString(s, ""), "{{") {
			return fmt.Errorf("only simple field actions allowed: %s", path)
		}
		fields := map[string]int{}
		for _, name := range allowed {
			f, ok := reflect.TypeFor[Data]().FieldByName(name)
			if !ok || f.Type.Kind() != reflect.String {
				return fmt.Errorf("invalid manifest field %s: %s", name, path)
			}
			fields[name] = f.Index[0]
		}
		parts := []part{}
		for _, node := range t.Tree.Root.Nodes {
			switch n := node.(type) {
			case *parse.TextNode:
				parts = append(parts, part{text: string(n.Text), field: -1})
			case *parse.ActionNode:
				p := n.Pipe
				if p.IsAssign || len(p.Decl) != 0 || len(p.Cmds) != 1 || len(p.Cmds[0].Args) != 1 {
					return fmt.Errorf("only simple fields allowed: %s", path)
				}
				f, ok := p.Cmds[0].Args[0].(*parse.FieldNode)
				if !ok || len(f.Ident) != 1 {
					return fmt.Errorf("only simple fields allowed: %s", path)
				}
				index, ok := fields[f.Ident[0]]
				if !ok {
					return fmt.Errorf("unknown field %s: %s", f.Ident[0], path)
				}
				parts = append(parts, part{field: index})
			default:
				return fmt.Errorf("restricted action in message template: %s", path)
			}
		}
		c[id] = parts
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id := range known {
		if _, ok := c[id]; !ok {
			return nil, fmt.Errorf("missing message template: %s", id)
		}
	}
	// Keyboard and exact-text routing must never disagree or become ambiguous.
	labels := map[string]bool{}
	for _, id := range []string{"clientmenu/label_invoice", "clientmenu/label_status", "clientmenu/label_configuration", "clientmenu/label_help"} {
		if _, ok := known[id]; !ok {
			continue // permits small, isolated catalogs in tests
		}
		label := c.render(id, Data{})
		if strings.TrimSpace(label) == "" || strings.HasPrefix(label, "/") || labels[label] {
			return nil, fmt.Errorf("empty, command-shaped or duplicate menu label: %s", id)
		}
		labels[label] = true
	}
	return c, nil
}

// Render accepts only display strings. Unknown literal IDs are programmer errors;
// callers and the complete manifest are covered by tests. Substitution is plain
// text, never HTML, and substituted strings are never interpreted as templates.
func Render(id string, data Data) string { return loaded.render(id, data) }

func (c catalog) render(id string, data Data) string {
	parts, ok := c[id]
	if !ok {
		panic("unknown message template: " + id)
	}
	v := reflect.ValueOf(data)
	var b strings.Builder
	for _, p := range parts {
		if p.field < 0 {
			b.WriteString(p.text)
		} else {
			b.WriteString(v.Field(p.field).String())
		}
	}
	return b.String()
}
