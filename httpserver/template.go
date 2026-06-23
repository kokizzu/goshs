package httpserver

import (
	"bytes"
	"fmt"
	"maps"
	"strconv"
	"text/template"
)

// templatingEnabled reports whether payload templating is active for this server.
func (fs *FileServer) templatingEnabled() bool {
	return fs.Options != nil && fs.Options.Template
}

// templateContext builds the variable map exposed to payload templates. Built-in
// values (Proto/Port/Host/LHOST) are derived from the server config; any
// --tpl-var KEY=VALUE entries overlay and may override them. LHOST is only set
// automatically when a single concrete IP is bound; otherwise it must come from
// --tpl-var LHOST= (and a template referencing {{.LHOST}} errors until then).
func (fs *FileServer) templateContext() map[string]string {
	proto := "http"
	if fs.SSL {
		proto = "https"
	}
	ctx := map[string]string{
		"Proto": proto,
		"Port":  strconv.Itoa(fs.Port),
		"Host":  fs.IP,
	}
	if fs.IP != "" && fs.IP != "0.0.0.0" {
		ctx["LHOST"] = fs.IP
	}
	if fs.Options != nil {
		maps.Copy(ctx, fs.Options.TemplateVarsParsed)
	}
	return ctx
}

// renderTemplate executes content as a Go text/template against the server's
// template context. missingkey=error means referencing an unresolved variable
// (e.g. {{.LHOST}} with no bound IP and no --tpl-var) fails loudly rather than
// emitting a blank into the payload.
func (fs *FileServer) renderTemplate(content []byte) ([]byte, error) {
	tmpl, err := template.New("payload").Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, fs.templateContext()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
