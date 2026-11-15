#!/usr/bin/env python3
# Scans shipped sources for gc bd invocations and prints one JSON object per
# distinct argv ({"argv": [...], "src": "repo:path:line"}), with $VARS,
# ${...}, {{templates}}, <placeholders> and $(...) normalized to X.
#
# What it reads:
#   * text (sh, toml, md, tmpl, yaml, txt, py, json): `gc bd ...` and
#     `gc --rig X bd ...` / `gc --city X bd ...`, with backslash continuations
#     joined, and multi-line TOML strings / markdown fences read as the lines
#     they hold. A JSON value in braces ({"k":"v"}) is kept as one token.
#   * Python, by AST: every list literal ["gc", ..., "bd", ...] and every call
#     of a gc-bd argv builder — a function with *args that puts "bd" then
#     *args into a gc argv (bd_gc_args, bd_args, rig_bd_args, _gc_bd_args, ...),
#     found to a fixpoint across the repo. Non-constant arguments become X; a
#     *splat ends the argv.
# It drops prose (tokens ending in . , :), gc's own --rig/--city, and a value
# flag whose value was cut off with the line gets X. Rows whose first word is
# not a bd verb are printed to stderr as "unknown" for review, not pinned.
#
# usage (from the gascity repo root):
#   python3 cmd/gc/testdata/scan_gc_bd_invocations.py internal/bdflags/inventory \
#     "gascity|$PWD|" "gascity-packs|<path>|origin/main" "workflows|<path>|" \
#     "maintainer-city|<path>|HEAD" \
#     > cmd/gc/testdata/gc_bd_shipped_invocations.jsonl
import ast, json, os, re, shlex, subprocess, sys


numeric_flags = set()


def load_inventory(d):
    verbs, value_flags, parents = set(), set(), set()
    global numeric_flags
    for f in sorted(os.listdir(d)):
        if not f.endswith('.json'):
            continue
        for c in json.load(open(os.path.join(d, f)))['commands']:
            p = c['path']
            if p:
                words = p.split(' ')
                verbs.add(words[0])
                if len(words) > 1:
                    parents.add(words[0])
                if len(words) == 1:
                    verbs.update(c.get('aliases') or [])
            for fl in (c.get('local') or []) + (c.get('persistent') or []):
                if not fl.get('no_opt_def_val'):
                    value_flags.add('--' + fl['name'])
                    if fl.get('shorthand'):
                        value_flags.add('-' + fl['shorthand'])
                    if fl['type'] in ('int', 'int64', 'uint', 'float64'):
                        numeric_flags.add('--' + fl['name'])
                        if fl.get('shorthand'):
                            numeric_flags.add('-' + fl['shorthand'])
    # Verbs that run on their own as well as having subcommands.
    return verbs, value_flags, parents - {'comments', 'state', 'human', 'graph'}


verbs, value_flags, parents = load_inventory(sys.argv[1])
out, unknown = [], []

SCOPES = {
    'gascity': r'(internal/bootstrap/packs/|examples/|cmd/gc/.*\.(md|tmpl)$)',
    'maintainer-city': r'(orders/|formulas/|\.beads/formulas/|prompts/|agents/|rig-packs/|packs/|hooks/|city\.toml$|pack\.toml$)',
}


def files(label, repo, ref):
    if ref:
        names = subprocess.run(['git', '-C', repo, 'ls-tree', '-r', '--name-only', ref], capture_output=True, text=True).stdout.split('\n')
    else:
        names = subprocess.run(['git', '-C', repo, 'ls-files'], capture_output=True, text=True).stdout.split('\n')
    for n in names:
        if not n or not re.search(r'\.(sh|bash|toml|md|tmpl|py|txt|yaml|yml|json)$', n):
            continue
        if re.search(r'(_test\.(go|py|sh)$|/tests?/|testdata|fixtures|test_[^/]*\.(py|sh)$)', n):
            continue
        if label in SCOPES and not re.match(SCOPES[label], n):
            continue
        if ref:
            data = subprocess.run(['git', '-C', repo, 'show', f'{ref}:{n}'], capture_output=True).stdout.decode('utf-8', 'replace')
        else:
            try:
                data = open(os.path.join(repo, n), errors='replace').read()
            except OSError:
                continue
        yield n, data


term = re.compile(r'(\|\||&&|\||;|`|\)\s*$|\s>|\s2>|\s<|\s#\s|"""|\'\'\')')


def norm(seg):
    seg = re.sub(r'\$\([^()]*\)', 'X', seg)
    seg = re.sub(r'\$\{[^}]*\}', 'X', seg)
    seg = re.sub(r'\$[A-Za-z_][A-Za-z0-9_]*', 'X', seg)
    seg = re.sub(r'\{\{[^}]*\}\}', 'X', seg)
    seg = re.sub(r'<[^<>\s][^<>]*>', 'X', seg)
    return seg


def strip_gc_scope(toks):
    t2, i = [], 0
    while i < len(toks):
        if toks[i] in ('--rig', '--city'):
            i += 2
            continue
        if toks[i].startswith(('--rig=', '--city=')):
            i += 1
            continue
        t2.append(toks[i])
        i += 1
    return [t for t in t2 if t not in ('...', '…')]


def balanced_json(tok):
    # A brace token is a JSON value bd reads ({"k":"v"}), not shell syntax.
    if '{' not in tok and '}' not in tok:
        return True
    body = tok[tok.index('{'):] if '{' in tok else tok
    try:
        json.loads(body)
        return True
    except ValueError:
        return False


def emit(toks, src, prose_checks=True):
    toks = strip_gc_scope(toks)
    first = next((t for t in toks if not t.startswith('-')), None)
    if not toks or first is None or first in GC_OWN:
        return
    if first not in verbs:
        if re.fullmatch(r'[a-z][a-z0-9-]*', first):
            unknown.append({'argv': toks, 'src': src})
        return
    if any(c in ''.join(toks) for c in '()[]') or not all(balanced_json(t) for t in toks):
        return
    if prose_checks and any(t.endswith(('.', ',', ':')) or '. ' in t for t in toks):
        return  # prose
    if toks[-1] in value_flags:
        toks = toks + ['X']  # the value was cut off with the line
    # Placeholders where bd needs a shape: --set-metadata k=v, --metadata {...}.
    toks = list(toks)
    for j in range(len(toks) - 1):
        if toks[j] == '--set-metadata' and '=' not in toks[j + 1]:
            toks[j + 1] = 'k=' + toks[j + 1]
        if toks[j] == '--metadata' and not toks[j + 1].startswith(('{', '@')):
            toks[j + 1] = '{}'
        if toks[j] in numeric_flags and toks[j + 1] == 'X':
            toks[j + 1] = '1'  # a placeholder where bd parses a number
    if prose_checks and toks in (['sql'], ['import']):
        return  # prose: "gc bd sql reads", "gc bd import"
    pos = [t for t in toks if not t.startswith('-')]
    if prose_checks and pos and pos[0] in parents and len(pos) == 1:
        return  # "gc bd mol" in prose
    out.append({'argv': toks, 'src': src})


# gc bd subcommands gc serves itself, before any bd guard.
GC_OWN = {'release-if-current'}

GC_BD = re.compile(r'(?<![\w-])gc(?:\s+--(?:rig|city)(?:=\S+|\s+\S+))*\s+bd\s+')


def scan_text(label, name, data):
    data = data.replace('\\\n', ' ')
    for ln, line in enumerate(data.split('\n'), 1):
        for m in GC_BD.finditer(line):
            rest = line[m.end():]
            cut = term.search(rest)
            if cut:
                rest = rest[:cut.start()]
            rest = norm(rest).strip().rstrip('`').strip()
            rest = re.sub(r'["\']?\s*$', '', rest) if rest.count('"') % 2 else rest
            try:
                toks = shlex.split(rest)
            except ValueError:
                continue
            emit(toks, f'{label}:{name}:{ln}')


# --- Python, by AST ---------------------------------------------------------

def const_str(node):
    return node.value if isinstance(node, ast.Constant) and isinstance(node.value, str) else None


def argv_of(elts):
    """Tokens of an argv expression list: constants kept, others X, a splat
    ends it."""
    toks = []
    for e in elts:
        if isinstance(e, ast.Starred):
            break
        s = const_str(e)
        toks.append(s if s is not None else 'X')
    return toks


def call_name(call):
    f = call.func
    if isinstance(f, ast.Name):
        return f.id
    if isinstance(f, ast.Attribute):
        return f.attr
    return None


def find_builders(trees):
    """Functions with *args that forward "bd", *args into a gc argv, directly
    or through another builder. Returns name -> (number of fixed positional
    parameters before *args, the bd argv tokens the builder puts ahead of
    *args, e.g. ["list"] for a bd_list helper)."""
    builders = {}
    gc_builders = set()
    funcs = []
    for tree in trees:
        for node in ast.walk(tree):
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.args.vararg:
                funcs.append(node)

    def splat_at(seq, va):
        for i, e in enumerate(seq):
            if isinstance(e, ast.Starred) and isinstance(e.value, ast.Name) and e.value.id == va:
                return i
        return -1

    # gc argv builders: ["gc", ..., *args].
    for fn in funcs:
        va = fn.args.vararg.arg
        for n in ast.walk(fn):
            if isinstance(n, ast.List) and n.elts and const_str(n.elts[0]) == 'gc' and splat_at(n.elts, va) >= 0:
                gc_builders.add(fn.name)
    changed = True
    while changed:
        changed = False
        for fn in funcs:
            if fn.name in builders:
                continue
            va = fn.args.vararg.arg
            fixed = len(fn.args.posonlyargs) + len(fn.args.args)
            for n in ast.walk(fn):
                prefix = None
                if isinstance(n, ast.List) and n.elts and const_str(n.elts[0]) == 'gc':
                    i = splat_at(n.elts, va)
                    if i >= 0:
                        before = strip_gc_scope(argv_of(n.elts[1:i]))
                        if before[:1] == ['bd']:
                            prefix = before[1:]
                elif isinstance(n, ast.Call) and call_name(n) in builders:
                    i = splat_at(n.args, va)
                    cfixed, cprefix = builders[call_name(n)]
                    if i >= cfixed:
                        prefix = cprefix + argv_of(n.args[cfixed:i])
                elif isinstance(n, ast.Call) and call_name(n) in gc_builders:
                    i = splat_at(n.args, va)
                    if i >= 0:
                        # gc_args(city, "bd", *args): the fixed args are not
                        # constants; the argv starts at the "bd".
                        consts = [const_str(x) for x in n.args[:i]]
                        if 'bd' in consts:
                            j = consts.index('bd')
                            prefix = strip_gc_scope(argv_of(n.args[j + 1:i]))
                if prefix is not None:
                    builders[fn.name] = (fixed, strip_gc_scope(prefix))
                    changed = True
                    break
    return builders


def scan_python(label, sources):
    trees = {}
    for name, data in sources:
        try:
            trees[name] = ast.parse(data)
        except SyntaxError:
            continue
    builders = find_builders(trees.values())
    for name, tree in trees.items():
        for n in ast.walk(tree):
            src = f'{label}:{name}:{getattr(n, "lineno", 0)}'
            if isinstance(n, ast.List) and len(n.elts) >= 2 and const_str(n.elts[0]) == 'gc':
                toks = argv_of(n.elts[1:])
                toks = strip_gc_scope(toks)
                if toks and toks[0] == 'bd' and len(toks) > 1:
                    emit(toks[1:], src, prose_checks=False)
            elif isinstance(n, ast.Call) and call_name(n) in builders:
                fixed, prefix = builders[call_name(n)]
                if isinstance(n.func, ast.Attribute) and fixed > 0:
                    fixed -= 1  # self
                toks = prefix + argv_of(n.args[fixed:])
                if toks:
                    emit(toks, src, prose_checks=False)


for spec in sys.argv[2:]:
    label, repo, ref = spec.split('|')
    py = []
    for name, data in files(label, repo, ref or None):
        scan_text(label, name, data)
        if name.endswith('.py'):
            py.append((name, data))
    scan_python(label, py)

seen = {}
for o in out:
    seen.setdefault(json.dumps(o['argv']), o)
for k in sorted(seen):
    print(json.dumps(seen[k]))
useen = {}
for o in unknown:
    useen.setdefault(json.dumps(o['argv']), o)
for k in sorted(useen):
    print('unknown: ' + json.dumps(useen[k]), file=sys.stderr)
