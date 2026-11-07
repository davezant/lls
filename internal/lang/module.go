package lang

import (
	"path"
	"strings"
)

// StripSourceExt removes the longest matching source extension from name and
// reports whether one matched.
func (l *Lang) StripSourceExt(name string) (string, bool) {
	for _, ext := range l.srcExts {
		if strings.HasSuffix(name, ext) && len(name) > len(ext) {
			return name[:len(name)-len(ext)], true
		}
	}
	return name, false
}

// IsSource reports whether a file name has one of the spec's source extensions.
func (l *Lang) IsSource(name string) bool {
	_, ok := l.StripSourceExt(name)
	return ok
}

// ModuleFor derives the module name of a file from its slash-separated path
// relative to the indexed root. isIndex reports whether the file is a package
// index file (__init__.py, index.js, mod.rs). ok=false means the file should
// not be indexed (private module, unusable path).
func (l *Lang) ModuleFor(rel string) (module string, isIndex, ok bool) {
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	ms := l.Module
	if len(ms.DropSegments) > 0 {
		// Drop directory segments such as "src" or "@types" before deriving
		// the module, so "std/src/io/mod.rs" becomes "std/io/mod.rs".
		parts := strings.Split(rel, "/")
		kept := make([]string, 0, len(parts))
		for i, s := range parts {
			drop := false
			if i < len(parts)-1 {
				for _, d := range ms.DropSegments {
					if s == d {
						drop = true
						break
					}
				}
			}
			if !drop {
				kept = append(kept, s)
			}
		}
		rel = strings.Join(kept, "/")
	}
	var segs []string
	switch ms.Mode {
	case "file-ext":
		segs = strings.Split(rel, "/")
	case "dir":
		dir := path.Dir(rel)
		if dir == "." {
			return "", false, false
		}
		segs = strings.Split(dir, "/")
	case "package":
		segs = strings.Split(rel, "/")
		n := 1
		if strings.HasPrefix(segs[0], "@") && len(segs) > 1 {
			n = 2
		}
		if len(segs) <= n {
			// A file at the root of the packages directory: "pkg.d.ts".
			base, _ := l.StripSourceExt(segs[len(segs)-1])
			segs[len(segs)-1] = base
		} else {
			segs = segs[:n]
		}
	default: // "file"
		base, _ := l.StripSourceExt(rel)
		segs = strings.Split(base, "/")
		last := segs[len(segs)-1]
		for _, idx := range ms.IndexNames {
			if last == idx {
				segs = segs[:len(segs)-1]
				isIndex = true
				break
			}
		}
	}
	if len(segs) == 0 {
		return "", isIndex, false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", isIndex, false
		}
		if ms.Private && s[0] == '_' {
			return "", isIndex, false
		}
	}
	sep := ms.Separator
	if ms.Mode == "file-ext" {
		sep = "/"
	}
	return strings.Join(segs, sep), isIndex, true
}

// ResolveRelative resolves a relative module reference written inside the
// file whose module is base. For dotted languages (Python) leading dots walk
// up the package hierarchy; for path languages it is a path join.
func (l *Lang) ResolveRelative(base string, baseIsIndex bool, rel string) string {
	sep := l.Module.Separator
	if l.Module.Mode == "file-ext" || sep == "/" {
		dir := path.Dir(base)
		if dir == "." {
			return path.Clean(rel)
		}
		return path.Clean(dir + "/" + rel)
	}
	dots := len(rel) - len(strings.TrimLeft(rel, "."))
	rest := rel[dots:]
	parts := strings.Split(base, sep)
	up := dots
	if baseIsIndex {
		up--
	}
	if up > len(parts) {
		return rest
	}
	parts = parts[:len(parts)-up]
	if rest != "" {
		parts = append(parts, rest)
	}
	return strings.Join(parts, sep)
}

// DefaultAlias returns the alias a module is bound to when imported without
// an explicit alias, according to mode ("module" or "last").
func (l *Lang) DefaultAlias(module, mode string) string {
	if mode != "last" {
		return module
	}
	segs := strings.FieldsFunc(module, func(r rune) bool { return r == '/' || strings.ContainsRune(l.Module.Separator, r) })
	for i := len(segs) - 1; i >= 0; i-- {
		if l.aliasSkip != nil && l.aliasSkip.MatchString(segs[i]) {
			continue
		}
		return strings.NewReplacer("-", "_", ".", "_").Replace(segs[i])
	}
	return module
}

// ContainerModule is the synthetic module holding the members of container
// inside module (e.g. "collections.OrderedDict" or "net/http.Client").
func ContainerModule(module, container string) string {
	if module == "" {
		return container
	}
	return module + "." + container
}
