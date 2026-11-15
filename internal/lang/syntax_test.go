package lang

import (
	"strings"
	"testing"
)

func checkLang(t *testing.T, name, src string) []Diagnostic {
	t.Helper()
	all, err := LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	l := all[name]
	if l == nil || !l.HasSyntax() {
		t.Fatalf("no syntax spec for %s", name)
	}
	return l.Check([]byte(src))
}

func TestCheck(t *testing.T) {
	tests := []struct {
		lang, name, src string
		want            []string // substrings of the expected messages, in order
	}{
		// Python: indentation.
		{"python", "ok", "def f(x):\n    if x:\n        return [1,\n  2]\n    return 0\n\nclass A: pass\n", nil},
		{"python", "missing block", "def f():\nreturn 1\n", []string{"expected an indented block after line 1"}},
		{"python", "missing block at eof", "if x:\n", []string{"expected an indented block"}},
		{"python", "unexpected indent", "x = 1\n    y = 2\n", []string{"unexpected indent"}},
		{"python", "bad dedent", "if x:\n    a\n  b\n", []string{"unindent does not match"}},
		{"python", "comments and strings", "def f():\n    # :\n    s = \"\"\"\nnot code:\n\"\"\"\n    return s  # x:\n", nil},
		{"python", "continuation", "x = 1 + \\\n        2\ny = 3\n", nil},
		{"python", "unclosed paren", "print(1\nx = 2\n", []string{`"(" was never closed`}},
		{"python", "unterminated string", "s = 'abc\nx = 1\n", []string{"unterminated string"}},
		{"python", "mixed tabs", "if x:\n \ta\n", []string{"mixes tabs and spaces"}},

		// Brace languages.
		{"c", "ok", "int main(void) {\n  char *s = \"}\"; char c = '{';\n  /* } */ // {\n  return 0;\n}\n", nil},
		{"c", "unclosed brace", "int main() {\n  return 0;\n", []string{`"{" was never closed`}},
		{"c", "stray closer", "int x;\n}\n", []string{`unexpected "}": nothing to close`}},
		{"c", "mismatch", "int f() {\n  g(1];\n}\n", []string{`unexpected "]"`}},
		{"c", "missing paren", "int f() {\n  g(1;\n}\n", []string{`"(" was never closed`}},
		{"c", "unterminated comment", "int x; /* oops\n", []string{"unterminated comment"}},
		{"c", "digit separators", "long x = 0xaaaa'aaaa'aaaaLL + 1'000; char c = L'{';\n", nil},
		{"go", "raw string", "package p\nvar s = `{\n(`\nfunc f() {}\n", nil},
		{"rust", "lifetimes", "fn f<'a>(x: &'a str) -> char { let c = '}'; let e = '\\''; /* /* */ } */ r#\"{\"# ; c }\n", nil},
		{"rust", "unclosed", "fn f() {\n", []string{`"{" was never closed`}},
		{"javascript", "regex", "const r = /[(]\\)/g; const d = a / b / c;\nif (x) { y = `${a}{` }\n", nil},
		{"javascript", "jsx apostrophe", "const A = () => (<p>Don't {x}</p>);\n", nil},
		{"javascript", "unclosed", "function f() {\n  if (x) {\n}\n", []string{`"{" was never closed`}},

		{"c", "preprocessor branches", "#ifdef A\nint f(int a,\n#else\nint f(long a,\n#endif\n  int b);\n#ifdef __cplusplus\nextern \"C\" {\n#endif\n#if 0\n\n } ) don't\n\n#endif\n#error can't\n#if (A \\\n  || B)\n#endif\n#ifdef __cplusplus\n}\n#endif\n", nil},
		{"rust", "raw strings", "fn f() { let a = r\"\\?\\\"; let b = br\"{\"; let c = r#\"\"{\"#; }\n", nil},
		{"javascript", "nested templates", "const s = `a ${f(`b ${x} }`)} c`; if (y) { z() }\n", nil},
		{"javascript", "unclosed interpolation", "const s = `a ${f(1)\n", []string{`"${" was never closed`}},

		// Lua: keyword blocks.
		{"lua", "ok", "local function f(x)\n  if x then\n    for i = 1, 3 do print(i) end\n  elseif y then\n  end\n  repeat x = x - 1 until x == 0\n  return function() end\nend\n--[[ end ]] local s = [[end]]\n", nil},
		{"lua", "z escape", "local s = \"\\z\n  do\\\nend\"\nlocal t = {}\n", nil},
		{"lua", "missing end", "function f()\n  if x then\n    print(1)\nend\n", []string{`"function" was never closed (expected "end")`}},
		{"lua", "extra end", "print(1)\nend\n", []string{`unexpected "end"`}},
		{"lua", "end closes paren", "function f()\n  g(1\nend\n", []string{`"(" was never closed`}},
	}
	for _, tt := range tests {
		t.Run(tt.lang+"/"+tt.name, func(t *testing.T) {
			got := checkLang(t, tt.lang, tt.src)
			var msgs []string
			for _, d := range got {
				msgs = append(msgs, d.Message)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d diagnostics %q, want %d %q", len(got), msgs, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(msgs[i], w) {
					t.Errorf("diagnostic %d = %q, want it to contain %q", i, msgs[i], w)
				}
			}
		})
	}
}
