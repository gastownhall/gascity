# go-top-level-symbols.awk — emit the top-level declared symbols of gofmt'd Go.
#
# Usage: awk -v mode=symbols|tests -f go-top-level-symbols.awk FILE...
# Output: one "<path>\t<symbol>" record per declaration.
#
#   mode=symbols  every top-level func, method, type, const and var.
#                 Methods are emitted as "Receiver.Name"; everything else as
#                 the bare identifier, so a const that becomes a var is the
#                 same symbol rather than a spurious deletion.
#   mode=tests    only "func Test*(" — the vanished-test census population.
#
# WHY NOT go/ast: this runs against three git trees on every invocation and
# must work on a tree that does not compile (a half-resolved merge is exactly
# when it is needed). A parser that needs a buildable package is useless there;
# a line scanner over gofmt'd source is not.
#
# GOFMT IS THE CONTRACT. Every top-level declaration starts at column 0 and
# every grouped member sits at exactly one tab. The repository's fmt-check
# gate enforces that, so anchoring on column 0 is sound.
#
# THE ONE HARD CASE is a raw string literal, the only construct that can put
# arbitrary text at column 0 inside a function body — including Go source, of
# which this repository has plenty in test fixtures. Raw-string state is
# therefore tracked across lines, after stripping the escapes, interpreted
# strings, rune literals and line comments that would otherwise contribute
# stray backticks. Block comments are not tracked: a `/*` … `*/` containing an
# odd number of backticks would desynchronize the scan, which no file in the
# tree does today and which the self-test's fixture would catch as a diff.

# advanceLexState walks one line and leaves inRaw / inBlock holding the state
# the NEXT line starts in.
#
# A regex approximation is not good enough here, and the tree proves it: strip
# line comments with a regex and `select((.assignee // "") == $id)` inside a jq
# raw string eats the rest of the line, including the backtick that closes the
# string. Everything after that point in internal/config/workquery.go then looks
# like string content, its 22 declarations vanish from the census, and the guard
# reports them as symbols upstream deleted. A guard whose false positives read
# exactly like its true positives is worse than no guard.
#
# Only lines that can change the state pay for the scan: a line with no backtick
# and no block-comment marker cannot open or close either construct.
function advanceLexState(text, i, n, c) {
	n = length(text)
	i = 1
	while (i <= n) {
		c = substr(text, i, 1)
		if (inBlock) {
			if (c == "*" && substr(text, i + 1, 1) == "/") {
				inBlock = 0
				i += 2
				continue
			}
			i++
			continue
		}
		if (inRaw) {
			if (c == "`") {
				inRaw = 0
			}
			i++
			continue
		}
		if (c == "`") {
			inRaw = 1
			i++
			continue
		}
		if (c == "/" && substr(text, i + 1, 1) == "/") {
			return
		}
		if (c == "/" && substr(text, i + 1, 1) == "*") {
			inBlock = 1
			i += 2
			continue
		}
		if (c == "\"" || c == "'") {
			quote = c
			i++
			while (i <= n) {
				c = substr(text, i, 1)
				if (c == "\\") {
					i += 2
					continue
				}
				i++
				if (c == quote) {
					break
				}
			}
			continue
		}
		i++
	}
}

# emit prints one symbol record unless the name is blank or the blank
# identifier, which declares nothing addressable.
function emit(name) {
	if (name != "" && name != "_") {
		print FILENAME "\t" name
	}
}

# emitList splits a comma-separated declaration list and emits the identifier
# each entry declares. Only the first token of an entry is the name, so
# "a, b Type" yields a and b rather than a and "b Type".
function emitList(list, parts, i, n, ident) {
	n = split(list, parts, /,/)
	for (i = 1; i <= n; i++) {
		ident = parts[i]
		gsub(/^[ \t]+|[ \t]+$/, "", ident)
		sub(/[ \t].*/, "", ident)
		if (ident ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
			emit(ident)
		}
	}
}

FNR == 1 {
	inRaw = 0
	inBlock = 0
	group = ""
}

{
	line = $0
	skipLine = (inRaw || inBlock)

	if (index(line, "`") > 0 || index(line, "/*") > 0 || index(line, "*/") > 0 || inRaw || inBlock) {
		advanceLexState(line)
	}

	if (skipLine) {
		next
	}
}

mode == "tests" {
	if (match(line, /^func (Test[A-Za-z0-9_]*)\(/)) {
		name = line
		sub(/^func /, "", name)
		sub(/\(.*/, "", name)
		emit(name)
	}
	next
}

# Grouped declarations: `const (`, `var (`, `type (` … `)`.
group != "" {
	if (line ~ /^\)/) {
		group = ""
		next
	}
	# Exactly one tab of indent, then an identifier list. Deeper indents are
	# struct fields and interface methods, which are not top-level symbols.
	if (match(line, /^\t[A-Za-z_][A-Za-z0-9_]*/)) {
		member = substr(line, 2)
		sub(/[ \t]*=.*/, "", member)
		emitList(member)
	}
	next
}

/^(const|var|type) \(/ {
	group = "open"
	next
}

/^func / {
	if (match(line, /^func \(/)) {
		# Method: pull the receiver base type, then the method name.
		recv = line
		sub(/^func \(/, "", recv)
		sub(/\).*/, "", recv)
		gsub(/\*/, "", recv)
		sub(/\[.*/, "", recv)          # generic receiver: (s Store[T])
		gsub(/^[ \t]+|[ \t]+$/, "", recv)
		nrecv = split(recv, rparts, /[ \t]+/)
		base = rparts[nrecv]
		name = line
		sub(/^func \([^)]*\)[ \t]*/, "", name)
		sub(/[([].*/, "", name)
		gsub(/[ \t]/, "", name)
		if (base != "" && name != "") {
			emit(base "." name)
		}
		next
	}
	name = line
	sub(/^func /, "", name)
	sub(/[([].*/, "", name)
	gsub(/[ \t]/, "", name)
	emit(name)
	next
}

/^type [A-Za-z_]/ {
	name = line
	sub(/^type /, "", name)
	sub(/[ \t[].*/, "", name)
	emit(name)
	next
}

/^(const|var) [A-Za-z_]/ {
	decl = line
	sub(/^(const|var) /, "", decl)
	sub(/[ \t]*=.*/, "", decl)
	# `var a Type`, `var a, b Type` and `var a, b = f()` all reduce to the
	# same comma-separated identifier list.
	emitList(decl)
	next
}
