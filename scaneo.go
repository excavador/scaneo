package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

const (
	usageText = `SCANEO
    Generate Go code to convert database rows into arbitrary structs.

USAGE
    scaneo [options] <golang_import_path=golang_source_package_or_file>...

OPTIONS
    -o, -output
        Set the name of the generated file. Default is scans.go.

    -p, -package
        Set the package name for the generated file. Default is current
        directory name.

    -u, -unexport
        Generate unexported functions. Default is export all.

    -w, -whitelist
        Only include structs specified in case-sensitive, comma-delimited
        string.

    -v, -version
        Print version and exit.

    -h, -help
        Print help and exit.

EXAMPLES
    tables.go is a file that contains one or more struct declarations.

    Generate scan functions based on structs in tables.go.
        scaneo tables.go

    Generate scan functions and name the output file funcs.go
        scaneo -o funcs.go tables.go

    Generate scans.go with unexported functions.
        scaneo -u tables.go

    Generate scans.go with only struct Post and struct user.
        scaneo -w "Post,user" tables.go

NOTES
    Struct field names don't have to match database column names at all.
    However, the order of the types must match.

    Integrate this with go generate by adding this line to the top of your
    tables.go file.
        //go:generate scaneo $GOFILE
`
)

type fieldToken struct {
	Name string
	Type string
}

type structToken struct {
	Import   string
	Selector string
	Name     string
	Fields []fieldToken
}

type importMap map[string][]string

func main() {
	log.SetFlags(0)

	outFilename := flag.String("o", "scans.go", "")
	packName := flag.String("p", "current directory", "")
	unexport := flag.Bool("u", false, "")
	whitelist := flag.String("w", "", "")
	version := flag.Bool("v", false, "")
	help := flag.Bool("h", false, "")
	flag.StringVar(outFilename, "output", "scans.go", "")
	flag.StringVar(packName, "package", "current directory", "")
	flag.BoolVar(unexport, "unexport", false, "")
	flag.StringVar(whitelist, "whitelist", "", "")
	flag.BoolVar(version, "version", false, "")
	flag.BoolVar(help, "help", false, "")
	flag.Usage = func() { log.Println(usageText) } // call on flag error
	flag.Parse()

	if *help {
		// not an error, send to stdout
		// that way people can: scaneo -h | less
		fmt.Println(usageText)
		return
	}

	if *version {
		fmt.Println("scaneo version 1.2.0")
		return
	}

	if *packName == "current directory" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal("couldn't get working directory:", err)
		}

		*packName = filepath.Base(wd)
	}

	importmap, err := findFiles(flag.Args())
	if err != nil {
		log.Println("couldn't find files:", err)
		log.Fatal(usageText)
	}

	structToks := make([]structToken, 0, 8)
	for targetImport, targetPathSlice := range importmap {
		for _, targetPath := range targetPathSlice {
			toks, err := parseCode(targetImport, targetPath, *whitelist)
			if err != nil {
				log.Println(`"syntax error" - parser probably`)
				log.Fatal(err)
			}

			structToks = append(structToks, toks...)
		}
	}

	if err := genFile(*outFilename, *packName, *unexport, structToks); err != nil {
		log.Fatal("couldn't generate file:", err)
	}
}

func findFiles(paths []string) (importMap, error) {
	if len(paths) < 1 {
		return nil, errors.New("no starting paths")
	}

	// using map to prevent duplicate file path entries
	// in case the user accidently passes the same file path more than once
	// probably because of autocomplete
	files := make(map[string]map[string]bool)

	for _, target := range paths {
		targetComponents := strings.Split(target, "=")
		if len(targetComponents) != 2 {
			return nil, fmt.Errorf("broken target, expected <golang_import_path=golang_source_package_or_file>, you provided: %s", target)
		}
		targetImport, targetPath := targetComponents[0], targetComponents[1]
		info, err := os.Stat(targetPath)
		if err != nil {
			return nil, err
		}

		if _, found := files[targetImport]; !found {
			files[targetImport] = make(map[string]bool)
		}

		if !info.IsDir() {
			// add file path to files
			files[targetImport][targetPath] = true
			continue
		}

		filepath.Walk(targetPath, func(fp string, fi os.FileInfo, _ error) error {
			if fi.IsDir() {
				// will still enter directory
				return nil
			} else if fi.Name()[0] == '.' {
				return nil
			}

			// add file path to files
			files[targetImport][fp] = true
			return nil
		})
	}

	result := make(importMap)

	var importSlice []string
	for targetImport := range files {
		importSlice = append(importSlice, targetImport)
	}

	for _, targetImport := range importSlice {
		var paths []string
		for targetPath := range files[targetImport] {
			paths = append(paths, targetPath)
		}
		sort.Strings(paths)
		result[targetImport] = paths
	}

	return result, nil
}

func parseCode(targetImport string, source string, commaList string) ([]structToken, error) {
	wlist := make(map[string]struct{})
	if commaList != "" {
		wSplits := strings.Split(commaList, ",")
		for _, s := range wSplits {
			wlist[s] = struct{}{}
		}
	}

	structToks := make([]structToken, 0, 8)

	fset := token.NewFileSet()
	astf, err := parser.ParseFile(fset, source, nil, 0)
	if err != nil {
		return nil, err
	}

	var filter bool
	if len(wlist) > 0 {
		filter = true
	}

	var selectorExpr string
	{
		selectorList := strings.Split(targetImport, "/")
		selectorExpr = selectorList[len(selectorList) - 1]
	}

	//ast.Print(fset, astf)
	for _, decl := range astf.Decls {
		genDecl, isGeneralDeclaration := decl.(*ast.GenDecl)
		if !isGeneralDeclaration {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec, isTypeDeclaration := spec.(*ast.TypeSpec)
			if !isTypeDeclaration {
				continue
			}

			structType, isStructTypeDeclaration := typeSpec.Type.(*ast.StructType)
			if !isStructTypeDeclaration {
				continue
			}

			// found a struct in the source code!

			var structTok structToken
			structTok.Import = targetImport
			structTok.Selector = selectorExpr
			// filter logic
			if structName := typeSpec.Name.Name; !filter {
				// no filter, collect everything
				structTok.Name = structName
			} else if _, exists := wlist[structName]; filter && !exists {
				// if structName not in whitelist, continue
				continue
			} else if filter && exists {
				// structName exists in whitelist
				structTok.Name = structName
			}

			structTok.Fields = make([]fieldToken, 0, len(structType.Fields.List))

			// iterate through struct fields (1 line at a time)
			for _, fieldLine := range structType.Fields.List {
				fieldToks := make([]fieldToken, len(fieldLine.Names))

				// get field name (or names because multiple vars can be declared in 1 line)
				for i, fieldName := range fieldLine.Names {
					fieldToks[i].Name = parseIdent(fieldName)
				}

				var fieldType string

				// get field type
				switch typeToken := fieldLine.Type.(type) {
				case *ast.Ident:
					// simple types, e.g. bool, int
					fieldType = parseIdent(typeToken)
				case *ast.SelectorExpr:
					// struct fields, e.g. time.Time, sql.NullString
					fieldType = parseSelector(typeToken)
				case *ast.ArrayType:
					// arrays
					fieldType = parseArray(typeToken)
				case *ast.StarExpr:
					// pointers
					fieldType = parseStar(typeToken)
				}

				if fieldType == "" {
					continue
				}

				// apply type to all variables declared in this line
				for i := range fieldToks {
					fieldToks[i].Type = fieldType
				}

				structTok.Fields = append(structTok.Fields, fieldToks...)
			}

			structToks = append(structToks, structTok)
		}
	}

	return structToks, nil
}

func parseIdent(fieldType *ast.Ident) string {
	// return like byte, string, int
	return fieldType.Name
}

func parseSelector(fieldType *ast.SelectorExpr) string {
	// return like time.Time, sql.NullString
	ident, isIdent := fieldType.X.(*ast.Ident)
	if !isIdent {
		return ""
	}

	return fmt.Sprintf("%s.%s", parseIdent(ident), fieldType.Sel.Name)
}

func parseArray(fieldType *ast.ArrayType) string {
	// return like []byte, []time.Time, []*byte, []*sql.NullString
	var arrayType string

	switch typeToken := fieldType.Elt.(type) {
	case *ast.Ident:
		arrayType = parseIdent(typeToken)
	case *ast.SelectorExpr:
		arrayType = parseSelector(typeToken)
	case *ast.StarExpr:
		arrayType = parseStar(typeToken)
	}

	if arrayType == "" {
		return ""
	}

	return fmt.Sprintf("[]%s", arrayType)
}

func parseStar(fieldType *ast.StarExpr) string {
	// return like *bool, *time.Time, *[]byte, and other array stuff
	var starType string

	switch typeToken := fieldType.X.(type) {
	case *ast.Ident:
		starType = parseIdent(typeToken)
	case *ast.SelectorExpr:
		starType = parseSelector(typeToken)
	case *ast.ArrayType:
		starType = parseArray(typeToken)
	}

	if starType == "" {
		return ""
	}

	return fmt.Sprintf("*%s", starType)
}

func genFile(outFile, pkg string, unexport bool, toks []structToken) error {
	if len(toks) < 1 {
		return errors.New("no structs found")
	}

	fout, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer fout.Close()

	importSet := make(map[string]bool)
	for _, tok := range toks {
		importSet[tok.Import] = true
	}

	var importList []string
	for targetImport := range importSet {
		if targetImport == "" {
			continue
		}
		importList = append(importList, targetImport)
	}
	sort.Strings(importList)

	data := struct {
		PackageName string
		Import      []string
		Tokens      []structToken
		Visibility  string
	}{
		PackageName: pkg,
		Import:      importList,
		Visibility:  "S",
		Tokens:      toks,
	}

	if unexport {
		// func name will be scanFoo instead of ScanFoo
		data.Visibility = "s"
	}

	fnMap := template.FuncMap{"title": strings.Title}
	scansTmpl, err := template.New("scans").Funcs(fnMap).Parse(scansText)
	if err != nil {
		return err
	}

	if err := scansTmpl.Execute(fout, data); err != nil {
		return err
	}

	return nil
}
