#!/usr/bin/env python3
"""Dump the Python standard library as lls JSON Lines.

Source scanning cannot see modules implemented in C (builtins, math, sys,
_socket...) nor runtime aliases such as ``os.path``. This script imports each
stdlib module and records its public API using ``inspect``.

Usage:
    python3 -I scripts/introspect/python.py OUT.jsonl [extra_module ...]

Each output line is one record understood by ``lls build --jsonl``:

    {"m": "os.path", "k": "function", "n": "join", "d": "join(a, *p)", "doc": "..."}

Class members are written to the container "module#Class", which lls stores
as the synthetic module "module.Class".
"""

import importlib
import inspect
import io
import json
import pkgutil
import sys
import warnings

MAX_DOC = 300

# Modules that have side effects on import or are useless for completion.
SKIP = {
    "antigravity", "this", "idlelib", "turtledemo", "lib2to3", "tkinter.tix",
    "__main__", "__phello__", "__hello__", "_testcapi", "_testinternalcapi",
    "_testbuffer", "_testimportmultiple", "_testmultiphase", "_xxtestfuzz",
    "test", "pydoc_data", "ensurepip", "venv", "msilib", "winreg", "winsound",
}


def first_paragraph(doc):
    if not doc or not isinstance(doc, str):
        return ""
    doc = inspect.cleandoc(doc)
    para = []
    for line in doc.splitlines():
        if not line.strip():
            if para:
                break
            continue
        para.append(line.strip())
    text = " ".join(para)
    if len(text) > MAX_DOC:
        cut = text.rfind(" ", 0, MAX_DOC)
        text = text[: cut if cut > MAX_DOC // 2 else MAX_DOC] + "…"
    return text


def signature(name, obj):
    try:
        return name + str(inspect.signature(obj))
    except (TypeError, ValueError):
        # Builtins without text signatures: use the first doc line when it
        # looks like a signature, e.g. "print(value, ..., sep=' ', end='\n')".
        doc = getattr(obj, "__doc__", None) or ""
        line = doc.strip().splitlines()[0] if doc.strip() else ""
        if line.startswith(name + "("):
            return line
        return name + "(...)"


def kind_of(obj):
    if inspect.isclass(obj):
        return "class"
    if inspect.ismodule(obj):
        return "module"
    if inspect.isroutine(obj):
        return "function"
    return "variable"


def describe(module_name, name, obj, out, container=None):
    kind = kind_of(obj)
    if kind == "function":
        detail = ("def " if container is None else "def ") + signature(name, obj)
        if container is not None:
            kind = "method"
    elif kind == "class":
        detail = "class " + name
        try:
            sig = inspect.signature(obj)
            detail += str(sig)
        except (TypeError, ValueError):
            pass
    elif kind == "module":
        detail = "module " + getattr(obj, "__name__", name)
    else:
        if name.isupper():
            kind = "constant"
        try:
            r = repr(obj)
        except Exception:
            r = "?"
        if len(r) > 60:
            r = type(obj).__name__
        detail = f"{name} = {r}" if kind == "constant" else f"{name}: {type(obj).__name__}"
        if container is not None:
            kind = "property" if kind == "variable" else kind
    doc = ""
    if kind in ("function", "method", "class", "module"):
        doc = first_paragraph(getattr(obj, "__doc__", None))
    rec = {"m": module_name if container is None else f"{module_name}#{container}",
           "k": kind, "n": name, "d": detail}
    if doc:
        rec["doc"] = doc
    out.write(json.dumps(rec, ensure_ascii=False) + "\n")


def public_names(mod):
    names = getattr(mod, "__all__", None)
    if names is None:
        names = [n for n in dir(mod) if not n.startswith("_")]
    return [n for n in names if isinstance(n, str)]


def dump_module(name, mod, out, seen):
    if name in seen:
        return
    seen.add(name)
    out.write(json.dumps({"m": name}) + "\n")
    for attr in public_names(mod):
        try:
            obj = getattr(mod, attr)
        except Exception:
            continue
        describe(name, attr, obj, out)
        if inspect.isclass(obj):
            for mname, mobj in inspect.getmembers(obj):
                if mname.startswith("_") and mname != "__init__":
                    continue
                if mname == "__init__":
                    continue
                try:
                    describe(name, mname, mobj, out, container=attr)
                except Exception:
                    pass
        elif inspect.ismodule(obj) and getattr(obj, "__name__", "").split(".")[0] in sys.stdlib_module_names:
            # Runtime aliases such as os.path (really posixpath).
            alias = f"{name}.{attr}"
            if alias not in seen and alias.count(".") < 3:
                dump_module(alias, obj, out, seen)


def iter_stdlib():
    names = sorted(getattr(sys, "stdlib_module_names", ()))
    if not names:  # Python < 3.10
        names = sorted(set(sys.builtin_module_names) |
                       {m.name for m in pkgutil.iter_modules() if m.module_finder and "site-packages" not in str(m.module_finder)})
    names = ["builtins"] + [n for n in names if n != "builtins"]
    for n in names:
        if n in SKIP or (n.startswith("_") and n != "__future__"):
            continue
        yield n


def main():
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    out_path = sys.argv[1]
    extra = sys.argv[2:]
    warnings.simplefilter("ignore")
    seen = set()
    real_stdout = sys.stdout
    with open(out_path, "w", encoding="utf-8") as out:
        todo = list(iter_stdlib()) + extra
        for name in todo:
            # Silence modules that print on import.
            sys.stdout = io.StringIO()
            try:
                mod = importlib.import_module(name)
            except BaseException:
                continue
            finally:
                sys.stdout = real_stdout
            try:
                dump_module(name, mod, out, seen)
            except Exception as exc:  # keep going: one bad module is not fatal
                print(f"skip {name}: {exc}", file=sys.stderr)
            # Sub-modules of packages (json.decoder, email.mime.text, ...).
            if hasattr(mod, "__path__"):
                for info in pkgutil.walk_packages(mod.__path__, name + "."):
                    sub = info.name
                    if any(part.startswith("_") for part in sub.split(".")) or \
                            any(sub == s or sub.startswith(s + ".") for s in SKIP) or ".test" in sub:
                        continue
                    sys.stdout = io.StringIO()
                    try:
                        submod = importlib.import_module(sub)
                    except BaseException:
                        continue
                    finally:
                        sys.stdout = real_stdout
                    try:
                        dump_module(sub, submod, out, seen)
                    except Exception as exc:
                        print(f"skip {sub}: {exc}", file=sys.stderr)
    print(f"{len(seen)} modules -> {out_path}", file=sys.stderr)


if __name__ == "__main__":
    main()
