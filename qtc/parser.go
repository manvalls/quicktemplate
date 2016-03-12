package main

import (
	"bytes"
	"fmt"
	goparser "go/parser"
	"io"
	"strings"
)

type parser struct {
	s                 *scanner
	w                 io.Writer
	packageName       string
	prefix            string
	forDepth          int
	skipOutputDepth   int
	importsUseEmitted bool
}

func parse(w io.Writer, r io.Reader, filePath, packageName string) error {
	p := &parser{
		s:           newScanner(r, filePath),
		w:           w,
		packageName: packageName,
	}
	return p.parseTemplate()
}

func (p *parser) parseTemplate() error {
	s := p.s
	p.Printf("package %s\n", p.packageName)
	p.Printf(`import (
	"io"

	"github.com/valyala/quicktemplate"
)
`)
	nonimportEmitted := false
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			// just skip top-level text
		case tagName:
			switch string(t.Value) {
			case "import":
				if nonimportEmitted {
					return fmt.Errorf("imports must be at the top of the template. Found at %s", s.Context())
				}
				if err := p.parseImport(); err != nil {
					return err
				}
			case "code":
				p.emitImportsUse()
				if err := p.parseTemplateCode(); err != nil {
					return err
				}
				nonimportEmitted = true
			case "func":
				p.emitImportsUse()
				if err := p.parseFunc(); err != nil {
					return err
				}
				nonimportEmitted = true
			default:
				return fmt.Errorf("unexpected tag found outside func: %q at %s", t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found %s outside func at %s", t, s.Context())
		}
	}
	p.emitImportsUse()
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse template: %s", err)
	}
	return nil
}

func (p *parser) emitImportsUse() {
	if p.importsUseEmitted {
		return
	}
	p.Printf(`var (
	_ = io.Copy
	_ = quicktemplate.AcquireByteBuffer
)
`)
	p.importsUseEmitted = true
}

func (p *parser) parseFunc() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	funcStr := "func " + string(t.Value)
	f, err := parseFuncDef(t.Value)
	if err != nil {
		return fmt.Errorf("error in %q at %s: %s", funcStr, s.Context(), err)
	}
	p.emitFuncStart(f)
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", funcStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfunc":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.emitFuncEnd(f)
				return nil
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", funcStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", funcStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", funcStr, err)
	}
	return fmt.Errorf("cannot find endfunc tag for %q at %s", funcStr, s.Context())
}

func (p *parser) parseFor() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	forStr := "for " + string(t.Value)
	if err = validateForStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", forStr, s.Context(), err)
	}
	p.Printf("for %s {", t.Value)
	p.prefix += "\t"
	p.forDepth++
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", forStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfor":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.forDepth--
				p.prefix = p.prefix[1:]
				p.Printf("}")
				return nil
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", forStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", forStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", forStr, err)
	}
	return fmt.Errorf("cannot find endfor tag for %q at %s", forStr, s.Context())
}

func (p *parser) parseIf() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	if len(t.Value) == 0 {
		return fmt.Errorf("empty if condition at %s", s.Context())
	}
	ifStr := "if " + string(t.Value)
	if err = validateIfStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", ifStr, s.Context(), err)
	}
	p.Printf("if %s {", t.Value)
	p.prefix += "\t"
	elseUsed := false
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", ifStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endif":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("}")
				return nil
			case "else":
				if elseUsed {
					return fmt.Errorf("duplicate else branch found for %q at %s", ifStr, s.Context())
				}
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("} else {")
				p.prefix += "\t"
				elseUsed = true
			case "elseif":
				if elseUsed {
					return fmt.Errorf("unexpected elseif branch found after else branch for %q at %s",
						ifStr, s.Context())
				}
				t, err = expectTagContents(s)
				if err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("} else if %s {", t.Value)
				p.prefix += "\t"
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", ifStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", ifStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", ifStr, err)
	}
	return fmt.Errorf("cannot find endif tag for %q at %s", ifStr, s.Context())
}

func (p *parser) tryParseCommonTags(tagBytes []byte) (bool, error) {
	s := p.s
	tagNameStr := string(tagBytes)
	switch tagNameStr {
	case "s", "v", "d", "f", "q", "z", "s=", "v=", "d=", "f=", "q=", "z=":
		t, err := expectTagContents(s)
		if err != nil {
			return false, err
		}
		filter := "N()."
		if len(tagNameStr) == 1 {
			switch tagNameStr {
			case "s", "v", "q", "z":
				filter = "E()."
			}
		} else {
			tagNameStr = tagNameStr[:len(tagNameStr)-1]
		}
		tagNameStr = strings.ToUpper(tagNameStr)
		p.Printf("qw.%s%s(%s)", filter, tagNameStr, t.Value)
	case "=":
		t, err := expectTagContents(s)
		if err != nil {
			return false, err
		}
		f, err := parseFuncCall(t.Value)
		if err != nil {
			return false, fmt.Errorf("error at %s: %s", s.Context(), err)
		}
		p.Printf("%s", f.CallStream("qw"))
	case "return":
		if err := p.skipAfterTag("return"); err != nil {
			return false, err
		}
	case "break":
		if p.forDepth <= 0 {
			return false, fmt.Errorf("found break tag outside for loop")
		}
		if err := p.skipAfterTag("break"); err != nil {
			return false, err
		}
	case "code":
		if err := p.parseFuncCode(); err != nil {
			return false, err
		}
	case "for":
		if err := p.parseFor(); err != nil {
			return false, err
		}
	case "if":
		if err := p.parseIf(); err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	return true, nil
}

func (p *parser) skipAfterTag(tagStr string) error {
	s := p.s
	if err := skipTagContents(s); err != nil {
		return err
	}
	p.Printf("%s", tagStr)
	p.skipOutputDepth++
	defer func() {
		p.skipOutputDepth--
	}()
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			// skip text
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error when parsing contents after %q: %s", tagStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfunc", "endfor", "endif", "else", "elseif":
				s.Rewind()
				return nil
			default:
				return fmt.Errorf("unexpected tag found after %q: %q at %s", tagStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing contents after %q: %s at %s", tagStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse contents after %q: %s", tagStr, err)
	}
	return fmt.Errorf("cannot find closing tag after %q at %s", tagStr, s.Context())
}

func (p *parser) parseImport() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	if len(t.Value) == 0 {
		return fmt.Errorf("empty import found at %s", p.s.Context())
	}
	p.Printf("import %s\n", t.Value)
	return nil
}

func (p *parser) parseTemplateCode() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	p.Printf("%s\n", t.Value)
	return nil
}

func (p *parser) parseFuncCode() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	if err = validateFuncCode(t.Value); err != nil {
		return fmt.Errorf("invalid code at %s: %s", p.s.Context(), err)
	}
	p.Printf("%s\n", t.Value)
	return nil
}

func (p *parser) emitText(text []byte) {
	for len(text) > 0 {
		n := bytes.IndexByte(text, '`')
		if n < 0 {
			p.Printf("qw.N().S(`%s`)", text)
			return
		}
		p.Printf("qw.N().S(`%s`)", text[:n])
		p.Printf("qw.N().S(\"`\")")
		text = text[n+1:]
	}
}

func (p *parser) emitFuncStart(f *funcType) {
	p.Printf("func %s {", f.DefStream("qw"))
	p.prefix = "\t"
}

func (p *parser) emitFuncEnd(f *funcType) {
	p.prefix = ""
	p.Printf("}\n")

	p.Printf("func %s {", f.DefWrite("qww"))
	p.prefix = "\t"
	p.Printf("qw := quicktemplate.AcquireWriter(qww)")
	p.Printf("%s", f.CallStream("qw"))
	p.Printf("quicktemplate.ReleaseWriter(qw)")
	p.prefix = ""
	p.Printf("}\n")

	p.Printf("func %s {", f.DefString())
	p.prefix = "\t"
	p.Printf("qb := quicktemplate.AcquireByteBuffer()")
	p.Printf("%s", f.CallWrite("qb"))
	p.Printf("qs := string(qb.B)")
	p.Printf("quicktemplate.ReleaseByteBuffer(qb)")
	p.Printf("return qs")
	p.prefix = ""
	p.Printf("}\n")
}

func (p *parser) Printf(format string, args ...interface{}) {
	if p.skipOutputDepth > 0 {
		return
	}
	w := p.w
	fmt.Fprintf(w, "%s", p.prefix)
	p.s.WriteLineComment(w)
	fmt.Fprintf(w, "%s", p.prefix)
	fmt.Fprintf(w, format, args...)
	fmt.Fprintf(w, "\n")
}

func skipTagContents(s *scanner) error {
	_, err := expectTagContents(s)
	return err
}

func expectTagContents(s *scanner) (*token, error) {
	return expectToken(s, tagContents)
}

func expectToken(s *scanner, id int) (*token, error) {
	if !s.Next() {
		return nil, fmt.Errorf("cannot find token %s: %v", tokenIDToStr(id), s.LastError())
	}
	t := s.Token()
	if t.ID != id {
		return nil, fmt.Errorf("unexpected token found %s. Expecting %s at %s", t, tokenIDToStr(id), s.Context())
	}
	return t, nil
}

func validateForStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { for %s {} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateIfStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { if %s {} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateFuncCode(code []byte) error {
	exprStr := fmt.Sprintf("func () { for { %s } }", code)
	_, err := goparser.ParseExpr(exprStr)
	return err
}
