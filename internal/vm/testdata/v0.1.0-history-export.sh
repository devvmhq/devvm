devvm_opencode_clean() {
  local s c left
  rm -f clean.db clean.db-journal clean.db-wal clean.db-shm &&
  sqlite3 -bail -cmd 'PRAGMA trusted_schema = OFF' -cmd '.timeout 10000' "$1" "VACUUM INTO 'clean.db'" >/dev/null &&
  s=$(sqlite3 -bail -cmd 'PRAGMA trusted_schema = OFF' clean.db 'SELECT printf('\''DROP TRIGGER IF EXISTS "%w";'\'', name) FROM sqlite_master WHERE type = '\''trigger'\'';
SELECT printf('\''DELETE FROM "%w";'\'', name) FROM sqlite_master WHERE type = '\''table'\'' AND name NOT LIKE '\''sqlite\_%'\'' ESCAPE '\''\'\'' AND (name LIKE '\''%credential%'\'' OR name LIKE '\''%account%'\'' OR name LIKE '\''%auth%'\'' OR name LIKE '\''%secret%'\'' OR name LIKE '\''%token%'\'' OR name LIKE '\''%share%'\'' OR name IN (SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) c WHERE m.type = '\''table'\'' AND lower(c.name) IN ('\''secret'\'', '\''access_token'\'', '\''refresh_token'\'', '\''id_token'\'', '\''api_key'\'', '\''apikey'\'', '\''password'\'', '\''token'\'', '\''credential'\'', '\''credentials'\'')));
SELECT '\''DELETE FROM migration WHERE id = '\'''\''20260805200742_import_legacy_credentials'\'''\'';'\'' FROM sqlite_master WHERE type = '\''table'\'' AND name = '\''migration'\'';
SELECT '\''DELETE FROM kv WHERE key LIKE '\'''\''wellknown:config:%'\'''\'';'\'' FROM sqlite_master WHERE type = '\''table'\'' AND name = '\''kv'\'';') &&
  printf 'PRAGMA journal_mode = DELETE;\nPRAGMA secure_delete = ON;\n%s\nVACUUM;\n' "$s" | sqlite3 -bail -cmd 'PRAGMA trusted_schema = OFF' clean.db >/dev/null &&
  c=$(sqlite3 -bail -cmd 'PRAGMA trusted_schema = OFF' clean.db 'SELECT '\''SELECT count(*) FROM sqlite_master WHERE type = '\'''\''trigger'\'''\'';'\'';
SELECT printf('\''SELECT count(*) FROM "%w";'\'', name) FROM sqlite_master WHERE type = '\''table'\'' AND name NOT LIKE '\''sqlite\_%'\'' ESCAPE '\''\'\'' AND (name LIKE '\''%credential%'\'' OR name LIKE '\''%account%'\'' OR name LIKE '\''%auth%'\'' OR name LIKE '\''%secret%'\'' OR name LIKE '\''%token%'\'' OR name LIKE '\''%share%'\'' OR name IN (SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) c WHERE m.type = '\''table'\'' AND lower(c.name) IN ('\''secret'\'', '\''access_token'\'', '\''refresh_token'\'', '\''id_token'\'', '\''api_key'\'', '\''apikey'\'', '\''password'\'', '\''token'\'', '\''credential'\'', '\''credentials'\'')));
SELECT '\''SELECT count(*) FROM kv WHERE key LIKE '\'''\''wellknown:config:%'\'''\'';'\'' FROM sqlite_master WHERE type = '\''table'\'' AND name = '\''kv'\'';') &&
  left=$(printf '%s\n' "$c" | sqlite3 -bail -cmd 'PRAGMA trusted_schema = OFF' clean.db) || return 1
  case $left in *[!0[:space:]]*) return 1 ;; esac
  [ ! -e clean.db-wal ] && [ ! -e clean.db-journal ]
}
mkdir -p ~/.cache && t=$(mktemp -d ~/.cache/devvm-export.XXXXXX) || exit 1
trap 'rm -rf "$t"' EXIT
mkdir "$t/devvm-clean" && cd "$t" || exit 1
add=()
for db in "$HOME"/'.local/share/opencode'/opencode*.db; do
  [ -f "$db" ] && [ ! -L "$db" ] || continue
  n=${db##*/}
  if ! command -v sqlite3 >/dev/null; then echo "devvm: left out $db: the VM has no sqlite3" >&2
  elif devvm_opencode_clean "$db" && mv clean.db "devvm-clean/$n" && add=(${add[@]+"${add[@]}"} "devvm-clean/$n"); then continue
  else echo "devvm: left out $db: its copy without opencode's logins failed" >&2
  fi
  :
done
[ "${#add[@]}" -eq 0 ] || add=(-C "$t" "${add[@]}")
cd ~ && ls -d .claude .claude.json .codex .local/share/opencode .local/state/opencode 2>/dev/null | tar -czf - '--exclude=.claude/.credentials.json' '--exclude=.codex/auth.json' '--exclude=.local/share/opencode/auth.json' '--exclude=.local/share/opencode/account.json' '--exclude=.local/share/opencode/mcp-auth.json' '--exclude=.local/share/opencode/bin' '--no-wildcards-match-slash' '--exclude=.local/share/opencode/opencode*.db' '--exclude=.local/share/opencode/opencode*.db-wal' '--exclude=.local/share/opencode/opencode*.db-shm' '--exclude=.local/share/opencode/opencode*.db-journal' '--transform=flags=r;s,^devvm-clean/,.local/share/opencode/,' -T - ${add[@]+"${add[@]}"}