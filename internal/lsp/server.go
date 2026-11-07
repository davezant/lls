package lsp

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"lls/internal/engine"
	"lls/internal/index"
	"lls/internal/text"
)

// Version is reported to clients in serverInfo.
var Version = "dev"

// Server is an LSP server bound to one engine and one connection.
type Server struct {
	e        *engine.Engine
	c        *conn
	logf     func(format string, args ...any)
	shutdown bool
	trace    bool

	// Last completion list, kept so completionItem/resolve can attach the
	// documentation lazily instead of sending it for every item.
	last []engine.Candidate
}

// docsInline is how many top items carry documentation in the completion
// response itself; the rest get it through completionItem/resolve.
const docsInline = 10

// Serve runs the server until the client sends "exit" or closes the stream.
func Serve(e *engine.Engine, r io.Reader, w io.Writer, logf func(string, ...any), trace bool) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{e: e, c: newConn(r, w), logf: logf, trace: trace}
	for {
		body, err := s.c.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var msg message
		if err := json.Unmarshal(body, &msg); err != nil {
			_ = s.c.replyError(nil, codeParseError, err.Error())
			continue
		}
		if msg.Method == "" {
			continue // a response to a server->client request; none are sent
		}
		start := time.Now()
		if msg.Method == "exit" {
			if !s.shutdown {
				return errors.New("lsp: exit without shutdown")
			}
			return nil
		}
		result, rerr := s.handle(&msg)
		if s.trace {
			s.logf("lls: %s %s", msg.Method, time.Since(start))
		}
		if len(msg.ID) == 0 { // notification: no response
			if rerr != nil {
				s.logf("lls: %s: %v", msg.Method, rerr)
			}
			continue
		}
		if rerr != nil {
			code := codeRequestFailed
			var ce *codeError
			if errors.As(rerr, &ce) {
				code = ce.code
			}
			err = s.c.replyError(msg.ID, code, rerr.Error())
		} else {
			if result == nil {
				result = []byte("null")
			}
			err = s.c.reply(msg.ID, result)
		}
		if err != nil {
			return err
		}
	}
}

type codeError struct {
	code int
	msg  string
}

func (e *codeError) Error() string { return e.msg }

// --- protocol types (only the fields lls uses) -----------------------------

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type rangeT struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type textDocumentID struct {
	URI     string `json:"uri"`
	Version int32  `json:"version"`
}

type positionParams struct {
	TextDocument textDocumentID `json:"textDocument"`
	Position     position       `json:"position"`
}

type initializeParams struct {
	RootURI          string `json:"rootUri"`
	RootPath         string `json:"rootPath"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
	InitializationOptions struct {
		Home  string `json:"home"`
		Limit int    `json:"limit"`
	} `json:"initializationOptions"`
}

func (s *Server) handle(msg *message) ([]byte, error) {
	switch msg.Method {
	case "initialize":
		return s.initialize(msg.Params)
	case "initialized", "$/cancelRequest", "$/setTrace", "workspace/didChangeConfiguration",
		"textDocument/didSave", "textDocument/willSave":
		return nil, nil
	case "shutdown":
		s.shutdown = true
		return nil, nil
	case "textDocument/didOpen":
		return nil, s.didOpen(msg.Params)
	case "textDocument/didChange":
		return nil, s.didChange(msg.Params)
	case "textDocument/didClose":
		var p struct {
			TextDocument textDocumentID `json:"textDocument"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, err
		}
		s.e.Close(p.TextDocument.URI)
		return nil, nil
	case "textDocument/completion":
		return s.completion(msg.Params)
	case "completionItem/resolve":
		return s.resolve(msg.Params)
	case "textDocument/hover":
		return s.hover(msg.Params)
	case "textDocument/signatureHelp":
		return s.signatureHelp(msg.Params)
	case "textDocument/definition":
		return s.definition(msg.Params)
	case "textDocument/documentSymbol":
		return s.documentSymbol(msg.Params)
	case "workspace/didChangeWorkspaceFolders":
		return nil, nil
	}
	if len(msg.ID) == 0 {
		return nil, nil // unknown notifications are ignored per spec
	}
	return nil, &codeError{codeMethodNotFound, "method not found: " + msg.Method}
}

func (s *Server) initialize(raw json.RawMessage) ([]byte, error) {
	var p initializeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &codeError{codeInvalidParams, err.Error()}
	}
	var roots []string
	for _, f := range p.WorkspaceFolders {
		roots = append(roots, engine.URIToPath(f.URI))
	}
	if len(roots) == 0 && p.RootURI != "" {
		roots = append(roots, engine.URIToPath(p.RootURI))
	} else if len(roots) == 0 && p.RootPath != "" {
		roots = append(roots, p.RootPath)
	}
	s.e.SetRoots(roots)
	s.e.SetLimit(p.InitializationOptions.Limit)

	// Trigger characters: the last byte of every member separator plus the
	// characters that open module paths.
	trig := map[string]bool{}
	for _, L := range s.e.Languages() {
		for _, sep := range L.MemberSeparators {
			trig[sep[len(sep)-1:]] = true
		}
	}
	for _, c := range []string{`"`, "'", "/", "<"} {
		trig[c] = true
	}
	b := []byte(`{"capabilities":{"positionEncoding":"utf-16","textDocumentSync":{"openClose":true,"change":2,"save":false},` +
		`"completionProvider":{"resolveProvider":true,"triggerCharacters":[`)
	first := true
	for _, c := range sortedKeys(trig) {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = appendString(b, c)
	}
	b = append(b, `]},"hoverProvider":true,"definitionProvider":true,"documentSymbolProvider":true,`+
		`"signatureHelpProvider":{"triggerCharacters":["(",","]}},"serverInfo":{"name":"lls","version":`...)
	b = appendString(b, Version)
	b = append(b, "}}"...)
	return b, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // tiny insertion sort, avoids importing sort
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Server) didOpen(raw json.RawMessage) error {
	var p struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
			Version    int32  `json:"version"`
			Text       string `json:"text"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	td := p.TextDocument
	if s.e.Open(td.URI, td.LanguageID, td.Version, []byte(td.Text)) == nil {
		s.logf("lls: no language for %s (%s)", td.URI, td.LanguageID)
	}
	return nil
}

func (s *Server) didChange(raw json.RawMessage) error {
	var p struct {
		TextDocument   textDocumentID `json:"textDocument"`
		ContentChanges []struct {
			Range *rangeT `json:"range"`
			Text  string  `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	d := s.e.Get(p.TextDocument.URI)
	if d == nil {
		return nil
	}
	for _, ch := range p.ContentChanges {
		if ch.Range == nil {
			d.Replace(p.TextDocument.Version, []byte(ch.Text))
			continue
		}
		start := d.Offset(ch.Range.Start.Line, ch.Range.Start.Character)
		end := d.Offset(ch.Range.End.Line, ch.Range.End.Character)
		d.Edit(p.TextDocument.Version, start, end, ch.Text)
	}
	return nil
}

func (s *Server) docAt(raw json.RawMessage) (*engine.Document, int, position, error) {
	var p positionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, 0, position{}, &codeError{codeInvalidParams, err.Error()}
	}
	d := s.e.Get(p.TextDocument.URI)
	if d == nil {
		return nil, 0, p.Position, nil
	}
	return d, d.Offset(p.Position.Line, p.Position.Character), p.Position, nil
}

func (s *Server) completion(raw json.RawMessage) ([]byte, error) {
	d, off, pos, err := s.docAt(raw)
	if err != nil || d == nil {
		return []byte(`{"isIncomplete":false,"items":[]}`), err
	}
	var ctx struct {
		Context *struct {
			TriggerKind int `json:"triggerKind"`
		} `json:"context"`
	}
	_ = json.Unmarshal(raw, &ctx)
	explicit := ctx.Context == nil || ctx.Context.TriggerKind == 1
	res := s.e.Complete(d, off, explicit)
	s.last = append(s.last[:0], res.Items...)

	// The replaced range starts where the typed prefix starts on this line.
	startChar := pos.Character - text.UTF16Len(d.Text[res.Start:off])
	if startChar < 0 {
		startChar = 0
	}
	rng := make([]byte, 0, 96)
	rng = append(rng, `{"start":{"line":`...)
	rng = strconv.AppendInt(rng, int64(pos.Line), 10)
	rng = append(rng, `,"character":`...)
	rng = strconv.AppendInt(rng, int64(startChar), 10)
	rng = append(rng, `},"end":{"line":`...)
	rng = strconv.AppendInt(rng, int64(pos.Line), 10)
	rng = append(rng, `,"character":`...)
	rng = strconv.AppendInt(rng, int64(pos.Character), 10)
	rng = append(rng, `}}`...)

	b := make([]byte, 0, 128*len(res.Items)+64)
	b = append(b, `{"isIncomplete":`...)
	b = strconv.AppendBool(b, res.Incomplete)
	b = append(b, `,"items":[`...)
	for i, it := range res.Items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"label":`...)
		b = appendString(b, it.Label)
		b = append(b, `,"kind":`...)
		b = strconv.AppendInt(b, int64(it.Kind), 10)
		if it.Detail != "" {
			b = append(b, `,"detail":`...)
			b = appendString(b, it.Detail)
		}
		if it.Doc != "" && i < docsInline {
			b = append(b, `,"documentation":{"kind":"markdown","value":`...)
			b = appendString(b, it.Doc)
			b = append(b, '}')
		}
		// sortText preserves lls' ranking in clients that re-sort.
		b = append(b, `,"sortText":"`...)
		b = appendPadded(b, i)
		b = append(b, `","textEdit":{"range":`...)
		b = append(b, rng...)
		b = append(b, `,"newText":`...)
		b = appendString(b, it.Label)
		b = append(b, `},"data":`...)
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, '}')
	}
	b = append(b, "]}"...)
	return b, nil
}

func appendPadded(b []byte, n int) []byte {
	var tmp [5]byte
	for i := 4; i >= 0; i-- {
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[:]...)
}

func (s *Server) resolve(raw json.RawMessage) ([]byte, error) {
	// Echo the item back, adding documentation when we still have it.
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, &codeError{codeInvalidParams, err.Error()}
	}
	var idx int
	var label string
	_ = json.Unmarshal(item["data"], &idx)
	_ = json.Unmarshal(item["label"], &label)
	if idx >= 0 && idx < len(s.last) && s.last[idx].Label == label && s.last[idx].Doc != "" {
		doc := appendString([]byte(`{"kind":"markdown","value":`), s.last[idx].Doc)
		item["documentation"] = append(doc, '}')
	}
	return json.Marshal(item)
}

func (s *Server) hover(raw json.RawMessage) ([]byte, error) {
	d, off, _, err := s.docAt(raw)
	if err != nil || d == nil {
		return nil, err
	}
	info, ok := s.e.Hover(d, off)
	if !ok || (info.Detail == "" && info.Doc == "") {
		return nil, nil
	}
	var md strings.Builder
	if info.Detail != "" {
		md.WriteString("```")
		md.WriteString(d.Lang.Name)
		md.WriteByte('\n')
		md.WriteString(info.Detail)
		md.WriteString("\n```")
	}
	if info.Module != "" && !strings.HasPrefix(info.Module, "file:") {
		md.WriteString("\n*")
		md.WriteString(info.Module)
		md.WriteString("*")
	}
	if info.Doc != "" {
		md.WriteString("\n\n")
		md.WriteString(info.Doc)
	}
	b := []byte(`{"contents":{"kind":"markdown","value":`)
	b = appendString(b, md.String())
	return append(b, "}}"...), nil
}

func (s *Server) signatureHelp(raw json.RawMessage) ([]byte, error) {
	d, off, _, err := s.docAt(raw)
	if err != nil || d == nil {
		return nil, err
	}
	sig, ok := s.e.SignatureHelp(d, off)
	if !ok {
		return nil, nil
	}
	b := []byte(`{"signatures":[{"label":`)
	b = appendString(b, sig.Label)
	if sig.Doc != "" {
		b = append(b, `,"documentation":{"kind":"markdown","value":`...)
		b = appendString(b, sig.Doc)
		b = append(b, '}')
	}
	b = append(b, `,"parameters":[`...)
	for i, p := range sig.Params {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"label":`...)
		b = appendString(b, p)
		b = append(b, '}')
	}
	b = append(b, `]}],"activeSignature":0,"activeParameter":`...)
	b = strconv.AppendInt(b, int64(sig.Active), 10)
	return append(b, '}'), nil
}

func (s *Server) definition(raw json.RawMessage) ([]byte, error) {
	d, off, _, err := s.docAt(raw)
	if err != nil || d == nil {
		return nil, err
	}
	loc, ok := s.e.Definition(d, off)
	if !ok {
		return nil, nil
	}
	b := []byte(`{"uri":`)
	b = appendString(b, engine.PathToURI(loc.Path))
	b = append(b, `,"range":`...)
	b = appendLineRange(b, loc.Line)
	return append(b, '}'), nil
}

func appendLineRange(b []byte, line int) []byte {
	b = append(b, `{"start":{"line":`...)
	b = strconv.AppendInt(b, int64(line), 10)
	b = append(b, `,"character":0},"end":{"line":`...)
	b = strconv.AppendInt(b, int64(line), 10)
	return append(b, `,"character":0}}`...)
}

func (s *Server) documentSymbol(raw json.RawMessage) ([]byte, error) {
	var p struct {
		TextDocument textDocumentID `json:"textDocument"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &codeError{codeInvalidParams, err.Error()}
	}
	d := s.e.Get(p.TextDocument.URI)
	if d == nil {
		return []byte("[]"), nil
	}
	b := []byte{'['}
	for i, def := range s.e.Symbols(d) {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendString(b, def.Name)
		b = append(b, `,"kind":`...)
		b = strconv.AppendInt(b, int64(symbolKind(def.Kind)), 10)
		if def.Container != "" {
			b = append(b, `,"containerName":`...)
			b = appendString(b, def.Container)
		}
		b = append(b, `,"location":{"uri":`...)
		b = appendString(b, d.URI)
		b = append(b, `,"range":`...)
		b = appendLineRange(b, def.Line)
		b = append(b, "}}"...)
	}
	return append(b, ']'), nil
}

// symbolKind maps a completion kind to the LSP SymbolKind enumeration.
func symbolKind(k index.Kind) int {
	switch k {
	case index.KindMethod:
		return 6
	case index.KindFunction:
		return 12
	case index.KindConstructor:
		return 9
	case index.KindField:
		return 8
	case index.KindVariable:
		return 13
	case index.KindClass:
		return 5
	case index.KindInterface:
		return 11
	case index.KindModule:
		return 2
	case index.KindProperty:
		return 7
	case index.KindEnum:
		return 10
	case index.KindEnumMember:
		return 22
	case index.KindConstant:
		return 14
	case index.KindStruct:
		return 23
	case index.KindTypeParameter:
		return 26
	}
	return 13
}
