#!/usr/bin/env bash
# Flags likely writing tropes in Markdown prose (see tropes.md). Code blocks,
# code spans, link targets, shortcodes, HTML comments and front matter are
# skipped. Hits are candidates to judge, not errors.
#
#   scan.sh FILE...        a table: words, sentences, then hits per pattern
#   scan.sh -v FILE...     each hit: file:line: [pattern] the line as written
#   scan.sh -p NAME FILE.. -v for one pattern only
#
# paras counts runs of prose paragraphs back to back, two or more on a user
# page and three or more in internals/ or contributing/, and long counts
# paragraphs and list items over 80 words or 4 sentences. -v, -p paras and
# -p long show each with its words. A colon-ended paragraph of 40 words or
# fewer introduces the block after it, so it doesn't count in a run.
#
# A .toml file is scanned as messages (locales/en.toml): only the other and
# one strings, with their {{.fields}} read as code spans. -v shows each hit's
# message ID, and -m REGEX scans only the messages whose ID matches.
set -euo pipefail

verbose=0
only=
ids=
while [ $# -gt 0 ]; do
	case $1 in
	-v) verbose=1 ;;
	-p)
		verbose=1
		only=${2:?-p needs a pattern name}
		shift
		;;
	-m)
		ids=${2:?-m needs a regular expression}
		shift
		;;
	-h | --help)
		sed -n '2,18s/^# \{0,1\}//p' "$0"
		exit 0
		;;
	--) shift && break ;;
	*) break ;;
	esac
	shift
done
[ $# -gt 0 ] || {
	echo "usage: scan.sh [-v | -p NAME] [-m REGEX] FILE..." >&2
	exit 2
}

# Markdown files or message files, not both: their tables have other columns.
mode=md
case $1 in *.toml) mode=toml ;; esac
for f in "$@"; do
	case $f in *.toml) m=toml ;; *) m=md ;; esac
	[ "$m" = "$mode" ] || {
		echo "scan.sh: scan .toml files apart from Markdown files" >&2
		exit 2
	}
done
[ -z "$ids" ] || [ "$mode" = toml ] || {
	echo "scan.sh: -m is for .toml files" >&2
	exit 2
}

# name<TAB>flags<TAB>regex: flags w (whole words), i (any case), - (none).
patterns='paren	-	\([^)]*[a-z][^)]*\)
colon	-	[a-z)`*]: [a-z]
semicolon	-	;
boldlabel	-	^[[:space:]]*([-*]|[0-9]+\.) \*\*[^*]+\*\*
negative	wi	isn.t|aren.t|doesn.t|don.t|won.t|can.t|never|nothing|neither|none|no one
too	-	 too[,.;:)]
say	i	, say\)|\(say,|, say,
dash	-	—| --
aiword	wi	additionally|crucial|delve|delves|enhance|enhances|fostering|garner|intricate|intricacies|landscape|meticulous|meticulously|pivotal|robust|seamless|seamlessly|showcase|showcases|tapestry|testament|underscore|underscores|vibrant|leverage|leverages|streamline|streamlines|effortless|effortlessly|comprehensive|powerful|essential|ensure|ensures|ensuring
copula	wi	serves as|stands as|acts as|functions as|operates as|boasts|plays a [a-z]+ role
notbut	i	not (just|only|merely)|rather than|it.s not [^,.]*, it.s
ingtail	i	, (ensuring|highlighting|underscoring|emphasizing|reflecting|allowing|enabling|letting|making)
summary	i	^(in (short|summary|conclusion)|overall|in other words|that said|put simply)[ ,]
hedge	wi	generally|typically|usually|essentially|basically|simply|just|really|actually|of course
token	wi	token|tokens'
if [ "$mode" = md ]; then
	patterns="$patterns
short	-	(^|[.!?] )(- |[0-9]+\. )?[^ .!?:;#|0-9-][^ .!?:;]*( [^ .!?:;]+){0,5}[.!?]( |\$)"
else
	patterns="$patterns
lower	-	^[a-z]
end	-	[^.?!:]\$"
fi

# The prose of one file, line numbers kept: lines that aren't prose are blank.
prose() {
	if [ "$mode" = toml ]; then
		messages "$1"
		return
	fi
	awk '
		NR == 1 && /^---$/ { front = 1; print ""; next }
		front { if (/^---$/) front = 0; print ""; next }
		/^[[:space:]]*(```|~~~)/ { fence = !fence; print ""; next }
		fence { print ""; next }
		/^[[:space:]]*\|[[:space:]:-]*\|[[:space:]:|-]*$/ { print ""; next }
		{
			gsub(/<!--.*-->/, "")
			gsub(/\{\{[<%][^}]*[>%]\}\}/, "")
			gsub(/`[^`]*`/, "`x`")
			gsub(/\]\([^)]*\)/, "]")
			print
		}' "$1"
}

# A message file's other and one strings, one per line, as prose: a \n is a
# space, a {{.field}} is a code span, and a leading devvm is a name, so a
# message that starts with it isn't flagged as lowercase.
messages() {
	awk -v ids="$ids" '
		/^\[/ { id = substr($0, 2, length($0) - 2); keep = ids == "" || id ~ ids; print ""; next }
		keep && /^(other|one) = "/ {
			s = $0
			sub(/^(other|one) = "/, "", s)
			sub(/"[[:space:]]*$/, "", s)
			gsub(/\\n/, " ", s)
			gsub(/\\"/, "\"", s)
			gsub(/\{\{[^}]*\}\}/, "`x`", s)
			sub(/^devvm /, "`x` ", s)
			print s
			next
		}
		{ print "" }' "$1"
}

# The line a hit is on, as written; a message's starts with its ID.
shown() { # file line
	if [ "$mode" = toml ]; then
		awk -v n="$2" '/^\[/ { id = $0 } NR == n { print id " " $0; exit }' "$1"
	else
		sed -n "$2p" "$1"
	fi
}

# The paragraphs of one file, for paras or long (what), one per line.
# paras: each run of prose paragraphs, as its first line, the count, each
# paragraph's words joined with + and the heading above it; a user page's runs
# start at two paragraphs, any other page's at three. long: each paragraph or
# list item over 80 words or 4 sentences, as its line, words, sentences and
# heading. Code fences, lists, tables, callouts, shortcodes, HTML and headings
# end a run, and a line that is only an HTML comment neither counts nor ends
# one. A paragraph of at most 40 words whose last line ends with a colon
# introduces the next block, so it doesn't count in a run.
paras() { # file what
	local min=2
	case $1 in */internals/* | */contributing/*) min=3 ;; esac
	awk -v what="$2" -v min="$min" '
		function flush() {
			if (what == "paras" && n >= min) printf "%d\t%d\t%s\t%s\n", start, n, sizes, head
			n = 0
			sizes = ""
		}
		function endblock(  t, w, s) {
			if (kind != "") {
				t = text
				gsub(/`[^`]*`/, "x", t)
				gsub(/\]\([^)]*\)/, "]", t)
				w = split(t, _, /[[:space:]]+/)
				if (t ~ /^[[:space:]]/) w--
				s = gsub(/[a-z0-9)x*][.!?]([[:space:]]|$)/, "&", t)
				if (what == "long" && kind != "o" && (w > 80 || s > 4))
					printf "%d\t%d\t%d\t%s\n", from, w, s, head
				if (kind == "p" && !(last ~ /:[[:space:]]*$/ && w <= 40)) {
					if (n == 0) start = from
					n++
					sizes = sizes (sizes == "" ? "" : "+") w
				}
			}
			kind = ""
			text = ""
		}
		function block(k) { endblock(); kind = k; from = NR; text = $0; last = $0 }
		BEGIN { head = "(no heading)" }
		NR == 1 && /^---$/ { front = 1; next }
		front { if (/^---$/) front = 0; next }
		/^[[:space:]]*(```|~~~)/ { endblock(); flush(); fence = !fence; next }
		fence { next }
		/^[[:space:]]*<!--.*-->[[:space:]]*$/ { next }
		/^[[:space:]]*$/ { endblock(); next }
		/^[[:space:]]*([-*+]|[0-9]+\.)[[:space:]]/ { block("l"); flush(); next }
		kind == "p" || kind == "l" { text = text " " $0; last = $0; next }
		kind == "o" { next }
		/^#/ { endblock(); flush(); head = $0; next }
		/^[[:space:]]*[|><]/ || /^[[:space:]]*\{\{/ { block("o"); flush(); next }
		/^[[:space:]]+[^[:space:]]/ { block("l"); flush(); next }
		{ block("p") }
		END { endblock(); flush() }' "$1"
}

hits() { # flags regex: matching lines (-n) or matches (-o) from stdin
	local flags=$1 re=$2 mode=$3 opts=-E
	case $flags in *w*) opts="$opts -w" ;; esac
	case $flags in *i*) opts="$opts -i" ;; esac
	# shellcheck disable=SC2086 # opts are flags, split on purpose
	grep $opts "$mode" -- "$re" || true
}

if [ "$verbose" = 0 ]; then
	printf '%-44s %6s %5s' file words sents
	while IFS='	' read -r name _ _; do printf ' %5.5s' "$name"; done <<EOF
$patterns
EOF
	[ "$mode" = toml ] || printf ' %5s %5s' paras long
	printf '\n'
fi

for f in "$@"; do
	text=$(prose "$f")
	if [ "$verbose" = 1 ]; then
		while IFS='	' read -r name flags re; do
			[ -z "$only" ] || [ "$only" = "$name" ] || continue
			printf '%s\n' "$text" | hits "$flags" "$re" -n |
				while IFS=: read -r n _; do
					printf '%s:%s: [%s] %s\n' "$f" "$n" "$name" "$(shown "$f" "$n")"
				done
		done <<EOF
$patterns
EOF
		if [ "$mode" = md ] && { [ -z "$only" ] || [ "$only" = paras ]; }; then
			paras "$f" paras | while IFS='	' read -r n count sizes head; do
				printf '%s:%s: [paras] %s paragraphs (%s words) under %s\n' "$f" "$n" "$count" "$sizes" "$head"
			done
		fi
		if [ "$mode" = md ] && { [ -z "$only" ] || [ "$only" = long ]; }; then
			paras "$f" long | while IFS='	' read -r n words sents head; do
				printf '%s:%s: [long] %s words, %s sentences under %s\n' "$f" "$n" "$words" "$sents" "$head"
			done
		fi
		continue
	fi
	words=$(printf '%s\n' "$text" | wc -w | tr -d ' ')
	sents=$(printf '%s\n' "$text" | hits - '[a-z)`][.!?]( |$)' -o | wc -l | tr -d ' ')
	printf '%-44s %6s %5s' "$f" "$words" "$sents"
	while IFS='	' read -r name flags re; do
		printf ' %5s' "$(printf '%s\n' "$text" | hits "$flags" "$re" -o | wc -l | tr -d ' ')"
	done <<EOF
$patterns
EOF
	[ "$mode" = toml ] || printf ' %5s %5s' "$(paras "$f" paras | wc -l | tr -d ' ')" \
		"$(paras "$f" long | wc -l | tr -d ' ')"
	printf '\n'
done
