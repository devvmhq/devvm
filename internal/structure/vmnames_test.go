package structure

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// A vmNameRule is where a VM's name is a vmname.VM (checkVMNames): the
// package that makes one from a string, the files outside it that read
// one back from what took it as one, each with why, and the folders it
// doesn't cover.
type vmNameRule struct {
	owner    string            // the package folder whose type it is
	converts map[string]string // file, from the module's root: why it converts
	free     []string          // folders, and theirs below
	// notNames are names isVMName takes for a VM's name that aren't
	// one, each with what it is.
	notNames map[string]string
}

// isVMName reports whether a field's, a parameter's or a result's name
// says it's a VM's name: vm or vms, VM or VMs, a name ending in VM or VMs
// after a lowercase letter (otherVM, but not KVM), or one starting with
// vm then a capital (vmName).
func isVMName(n string) bool {
	switch n {
	case "vm", "vms", "VM", "VMs":
		return true
	}
	for _, suffix := range []string{"VM", "VMs"} {
		if rest, ok := strings.CutSuffix(n, suffix); ok && rest != "" && unicode.IsLower(rune(rest[len(rest)-1])) {
			return true
		}
	}
	rest, ok := strings.CutPrefix(n, "vm")
	return ok && rest != "" && unicode.IsUpper(rune(rest[0]))
}

// stringly reports whether a type is a string, a list of them or a map
// keyed on them: what a VM's name, or several, would be untyped.
func stringly(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "string"
	case *ast.ArrayType:
		return stringly(x.Elt)
	case *ast.Ellipsis:
		return stringly(x.Elt)
	case *ast.StarExpr:
		return stringly(x.X)
	case *ast.MapType:
		return stringly(x.Key)
	}
	return false
}

// checkVMNames: outside tests, r.vmNames.owner and its free folders, a
// VM's name is a vmname.VM. A struct field, a parameter or a result whose
// name says it's a VM's (isVMName) and whose type is a string, a list of
// them or a map keyed on them fails, and so does a conversion to
// vmname.VM of anything but a literal outside the files converts lists:
// a name read from outside is parsed where it's read (vmname.Parse,
// vmname.ParseListed, or JSON's vmname.VM.UnmarshalText). A type alias
// of vmname.VM fails too, as conversions to it would pass.
//
// It checks no more than that: a VM's name in a string named otherwise
// (target, client, parent, to, name), vmname imported with a dot or
// reached through a type parameter, and a VM joined to a string constant
// (vm + lineNoLinks, whose type is vmname.VM) all pass.
func checkVMNames(m *module, r rules) []violation {
	o := r.vmNames
	if o.owner == "" {
		return nil
	}
	var vs []violation
	converted := map[string]bool{} // the converts files that convert, and the notNames there are
	for _, f := range m.files {
		if f.test || f.dir == o.owner || slices.ContainsFunc(o.free, func(d string) bool { return f.dir == d || strings.HasPrefix(f.dir, d+"/") }) {
			continue
		}
		pkg := importName(f, m.path+"/"+o.owner)
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && x.Assign.IsValid() && pkg != "" && sel.Sel.Name == "VM" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
						vs = append(vs, violation{ruleOwners, m.pos(x), x.Name.Name + " is an alias of vmname.VM: use vmname.VM (" + o.owner + ")"})
					}
				}
			case *ast.Field:
				if !stringly(x.Type) {
					return true
				}
				for _, name := range x.Names {
					if o.notNames[name.Name] != "" {
						converted[name.Name] = true
						continue
					}
					if isVMName(name.Name) {
						vs = append(vs, violation{ruleOwners, m.pos(name), fmt.Sprintf("%s is a VM's name: make it a vmname.VM (%s)", name.Name, o.owner)})
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || pkg == "" || sel.Sel.Name != "VM" || len(x.Args) != 1 {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != pkg {
					return true
				}
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return true
				}
				if o.converts[f.name] != "" {
					converted[f.name] = true
				} else {
					vs = append(vs, violation{ruleOwners, m.pos(x), "makes a vmname.VM of a string: parse it where it's read, with vmname.Parse or vmname.ParseListed (" + o.owner + ")"})
				}
			}
			return true
		})
	}
	for _, file := range slices.Sorted(maps.Keys(o.converts)) {
		if !converted[file] {
			vs = append(vs, violation{ruleOwners, "vmNames", file + " converts no string to a vmname.VM: take it out"})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(o.notNames)) {
		if !converted[name] {
			vs = append(vs, violation{ruleOwners, "vmNames", "nothing is named " + name + ": take it out of notNames"})
		}
	}
	return vs
}
