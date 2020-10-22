// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codec

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"reflect"
	"sort"
	"strings"
	"text/template"
)

// Generate write a Go file to w with definitions for encoding values using
// this package. It generates code for the type of each value in vs, as well
// as any types they depend on.
// packageName is the name following the file's package declaration.

func Generate(w io.Writer, packageName string, vs ...interface{}) error {
	g := &generator{
		pkg:  packageName,
		done: map[reflect.Type]bool{},
	}
	funcs := template.FuncMap{
		"funcName":   g.funcName,
		"goName":     g.goName,
		"encodeStmt": g.encodeStatement,
		"decodeStmt": g.decodeStatement,
	}

	newTemplate := func(name, body string) *template.Template {
		return template.Must(template.New(name).Delims("«", "»").Funcs(funcs).Parse(body))
	}

	g.initialTemplate = newTemplate("initial", initialBody)
	g.sliceTemplate = newTemplate("slice", sliceBody)
	g.mapTemplate = newTemplate("map", mapBody)
	g.structTemplate = newTemplate("struct", structBody)

	for _, v := range vs {
		g.todo = append(g.todo, reflect.TypeOf(v))
	}

	// Mark the types we already know about as done.
	for t := range typeInfosByType {
		g.done[t] = true
	}
	// The empty interface doesn't need any additional code. It's tricky to get
	// its reflect.Type: we need to dereference the pointer type.
	var iface interface{}
	g.done[reflect.TypeOf(&iface).Elem()] = true

	src, err := g.generate()
	if err != nil {
		return err
	}
	src, err = format.Source(src)
	if err != nil {
		return fmt.Errorf("format.Source: %v", err)
	}
	_, err = w.Write(src)
	return err
}

type generator struct {
	pkg             string
	todo            []reflect.Type
	done            map[reflect.Type]bool
	initialTemplate *template.Template
	sliceTemplate   *template.Template
	mapTemplate     *template.Template
	structTemplate  *template.Template
}

func (g *generator) generate() ([]byte, error) {
	importMap := map[string]bool{
		"golang.org/x/pkgsite/internal/godoc/codec": true,
	}
	var pieces [][]byte
	for len(g.todo) > 0 {
		t := g.todo[0]
		g.todo = g.todo[1:]
		if !g.done[t] {
			if t.PkgPath() != "" {
				importMap[t.PkgPath()] = true
			}
			code, err := g.gen(t)
			if err != nil {
				return nil, err
			}
			if code != nil {
				pieces = append(pieces, code)
			}
			// We use the same code for T and *T, so both are done.
			g.done[t] = true
			g.done[reflect.PtrTo(t)] = true
		}
	}

	var imports []string
	for i := range importMap {
		imports = append(imports, i)
	}
	sort.Strings(imports)
	result, err := execute(g.initialTemplate, struct {
		Package string
		Imports []string
	}{
		Package: g.pkg,
		Imports: imports,
	})
	if err != nil {
		return nil, err
	}
	for _, p := range pieces {
		result = append(result, p...)
	}
	return result, nil
}

func (g *generator) gen(t reflect.Type) ([]byte, error) {
	switch t.Kind() {
	case reflect.Slice:
		return g.genSlice(t)
	case reflect.Map:
		return g.genMap(t)
	case reflect.Struct:
		return g.genStruct(t)
	case reflect.Ptr:
		return g.gen(t.Elem())
	}
	return nil, nil
}

func (g *generator) genSlice(t reflect.Type) ([]byte, error) {
	et := t.Elem()
	g.todo = append(g.todo, et)
	return execute(g.sliceTemplate, struct {
		Type, ElType reflect.Type
	}{
		Type:   t,
		ElType: et,
	})
}

func (g *generator) genMap(t reflect.Type) ([]byte, error) {
	et := t.Elem()
	kt := t.Key()
	g.todo = append(g.todo, kt, et)
	return execute(g.mapTemplate, struct {
		Type, ElType, KeyType reflect.Type
	}{
		Type:    t,
		ElType:  et,
		KeyType: kt,
	})
}

func (g *generator) genStruct(t reflect.Type) ([]byte, error) {
	fields := exportedFields(t)
	for _, f := range fields {
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		g.todo = append(g.todo, ft)
	}
	return execute(g.structTemplate, struct {
		Type   reflect.Type
		Fields []field
	}{
		Type:   t,
		Fields: fields,
	})
}

// A field holds the information necessary to generate the encoder for a struct field.
type field struct {
	Name string
	Type reflect.Type
	Zero string // representation of the type's zero value
}

// exportedFields returns the exported fields of the struct type t that
// should be encoded, in the proper order.
// Exported fields of embedded, unexported types are not included.
func exportedFields(t reflect.Type) []field {
	var fs []field
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath == "" { // exported
			fs = append(fs, field{
				Name: f.Name,
				Type: f.Type,
				Zero: zeroValue(f.Type),
			})
		}
	}
	return fs
}

// zeroValue returns the string representation of a zero value of type t,
// or the empty string if there isn't one.
func zeroValue(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "false"
	case reflect.String:
		return `""`
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "0"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "0"
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return "0"
	case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
		return "nil"
	default:
		return ""
	}
}

func execute(tmpl *template.Template, data interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeStatement returns a Go statement that encodes a value denoted by arg, of type t.
func (g *generator) encodeStatement(t reflect.Type, arg string) string {
	bn, native := builtinName(t)
	if bn != "" {
		// t can be handled by an Encoder method.
		if t != native {
			// t is not the Encoder method's argument type, so we must cast.
			arg = fmt.Sprintf("%s(%s)", native, arg)
		}
		return fmt.Sprintf("e.Encode%s(%s)", bn, arg)
	}
	if t.Kind() == reflect.Interface {
		return fmt.Sprintf("e.EncodeAny(%s)", arg)
	}
	return fmt.Sprintf("encode_%s(e, %s)", g.funcName(t), arg)
}

func (g *generator) decodeStatement(t reflect.Type, arg string) string {
	bn, native := builtinName(t)
	if bn != "" {
		// t can be handled by a Decoder method.
		if t != native {
			// t is not the Decoder method's return type, so we must cast.
			return fmt.Sprintf("%s = %s(d.Decode%s())", arg, g.goName(t), bn)
		}
		return fmt.Sprintf("%s = d.Decode%s()", arg, bn)
	}
	if t.Kind() == reflect.Interface {
		// t is an interface, so use DecodeAny, possibly with a type assertion.
		if t.NumMethod() == 0 {
			return fmt.Sprintf("%s = d.DecodeAny()", arg)
		}
		return fmt.Sprintf("%s = d.DecodeAny().(%s)", arg, g.goName(t))
	}
	// Assume we will generate a decode method for t.
	return fmt.Sprintf("decode_%s(d, &%s)", g.funcName(t), arg)
}

// builtinName returns the suffix to append to "encode" or "decode" to get the
// Encoder/Decoder method name for t. If t cannot be encoded by an Encoder
// method, the suffix is "". The second return value is the "native" type of the
// method: the argument to the Encoder method, and the return value of the
// Decoder method.
func builtinName(t reflect.Type) (suffix string, native reflect.Type) {
	switch t.Kind() {
	case reflect.String:
		return "String", reflect.TypeOf("")
	case reflect.Bool:
		return "Bool", reflect.TypeOf(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "Int", reflect.TypeOf(int64(0))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "Uint", reflect.TypeOf(uint64(0))
	case reflect.Float32, reflect.Float64:
		return "Float64", reflect.TypeOf(0.0)
	case reflect.Slice:
		if t.Elem() == reflect.TypeOf(byte(0)) {
			return "Bytes", reflect.TypeOf([]byte(nil))
		}
	}
	return "", nil
}

// goName returns the name of t as it should appear in a Go program.
// E.g. "go/ast.File" => ast.File
// It assumes all package paths are represented in the file by their last element.
func (g *generator) goName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Slice:
		return fmt.Sprintf("[]%s", g.goName(t.Elem()))
	case reflect.Map:
		return fmt.Sprintf("map[%s]%s", g.goName(t.Key()), g.goName(t.Elem()))
	case reflect.Ptr:
		return fmt.Sprintf("*%s", g.goName(t.Elem()))
	default:
		s := t.String()
		if strings.HasPrefix(s, g.pkg+".") {
			s = s[len(g.pkg)+1:]
		}
		return s
	}
}

var funcNameReplacer = strings.NewReplacer("[]", "slice_", "[", "_", "]", "_", ".", "_", "*", "")

// funcName returns the name for t that is used as part of the encode/decode function name.
// E.g. "ast.File" => "ast_File".
func (g *generator) funcName(t reflect.Type) string {
	return funcNameReplacer.Replace(g.goName(t))
}

// Template body for the beginning of the file.
const initialBody = `
// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DO NOT MODIFY. Generated code.

package «.Package»

import (
	«range .Imports»
		"«.»"
	«end»
)

`

// Template body for a sliceBody type.
const sliceBody = `
« $funcName := funcName .Type »
« $goName := goName .Type »
func encode_«$funcName»(e *codec.Encoder, s «$goName») {
	if s == nil {
		e.EncodeUint(0)
		return
	}
	e.StartList(len(s))
	for _, x := range s {
		«encodeStmt .ElType "x"»
	}
}

func decode_«$funcName»(d *codec.Decoder, p *«$goName») {
	n := d.StartList()
	if n < 0 { return }
	s := make([]«goName .ElType», n)
	for i := 0; i < n; i++ {
		«decodeStmt .ElType "s[i]"»
	}
	*p = s
}

func init() {
  codec.Register(«$goName»(nil),
    func(e *codec.Encoder, x interface{}) { encode_«$funcName»(e, x.(«$goName»)) },
    func(d *codec.Decoder) interface{} { var x «$goName»; decode_«$funcName»(d, &x); return x })
}
`

// Template body for a map type.
// A nil map is encoded as a zero.
// A map of size N is encoded as a list of length 2N, containing alternating
// keys and values.
//
// In the decode function, we declare a variable v to hold the decoded map value
// rather than decoding directly into m[v]. This is necessary for decode
// functions that take pointers: you can't take a pointer to a map element.
const mapBody = `
« $funcName := funcName .Type »
« $goName := goName .Type »
func encode_«$funcName»(e *codec.Encoder, m «$goName») {
	if m == nil {
		e.EncodeUint(0)
		return
	}
	e.StartList(2*len(m))
	for k, v := range m {
		«encodeStmt .KeyType "k"»
		«encodeStmt .ElType "v"»
	}
}

func decode_«$funcName»(d *codec.Decoder, p *«$goName») {
	n2 := d.StartList()
	if n2 < 0 { return }
	n := n2/2
	m := make(«$goName», n)
	var k «goName .KeyType»
	var v «goName .ElType»
	for i := 0; i < n; i++ {
		«decodeStmt .KeyType "k"»
		«decodeStmt .ElType "v"»
		m[k] = v
	}
	*p = m
}

func init() {
	codec.Register(«$goName»(nil),
	func(e *codec.Encoder, x interface{}) { encode_«$funcName»(e, x.(«$goName»)) },
	func(d *codec.Decoder) interface{} { var x «$goName»; decode_«$funcName»(d, &x); return x })
}
`

// Template body for a (pointer to a) struct type.
// A nil pointer is encoded as a zero. (This is done in Encoder.StartStruct.)
// Otherwise, a struct is encoded as the start code, its exported fields, then
// the end code. Each non-zero field is encoded as its field number followed by
// its value. A field that equals its zero value isn't encoded.
const structBody = `
« $funcName := funcName .Type »
« $goName := goName .Type »
func encode_«$funcName»(e *codec.Encoder, x *«$goName») {
	if !e.StartStruct(x==nil) { return }
	«range $i, $f := .Fields»
		«- if $f.Zero -»
			if x.«$f.Name» != «$f.Zero» {
		«- end»
		e.EncodeUint(«$i»)
		«encodeStmt .Type (print "x." $f.Name)»
		«- if $f.Zero -»
		}
		«- end»
	«end -»
	e.EndStruct()
}

func decode_«$funcName»(d *codec.Decoder, p **«$goName») {
	if !d.StartStruct() { return }
	var x «$goName»
	for {
		n := d.NextStructField()
		if n < 0 { break }
		switch n {
		«range $i, $f := .Fields -»
			case «$i»:
	        «decodeStmt $f.Type (print "x." $f.Name)»
		«end»
		default:
			d.UnknownField("«$goName»", n)
		}
		*p = &x
	}
}

func init() {
	codec.Register(&«$goName»{},
		func(e *codec.Encoder, x interface{}) { encode_«$funcName»(e, x.(*«$goName»)) },
		func(d *codec.Decoder) interface{} {
			var x *«$goName»
			decode_«$funcName»(d, &x)
			return x
		})
}
`