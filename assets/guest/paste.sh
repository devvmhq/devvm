#!/bin/sh
# devvm paste's side in the VM, run as sh -c SCRIPT devvm-paste COMMAND DIR [NAME] with the
# script itself as the argument, so nothing of it is left on disk. The host names the folder DIR
# (config.toml's paste-dir) and the file NAME, both from a fixed set of characters.
#   put DIR NAME   writes stdin to DIR/NAME: to NAME.part first, mode 600, then renamed over NAME,
#                  so a reader never sees half of it. DIR is made, mode 700, when it's missing, and
#                  refused when it's a symbolic link, isn't a folder or isn't this user's.
#   count DIR      prints how many files devvm paste left in DIR
#   rm DIR         deletes them and prints how many
# count and rm see only regular files at DIR's top level whose names devvm paste gives, and its
# .part leftovers, never anything else in a folder that may be yours.
set -eu
cmd=$1 dir=$2
h='[0-9a-f]'
d='[0-9]'
pat="paste-$d$d$d$d$d$d$d$d-$d$d$d$d$d$d-$h$h$h$h$h$h$h$h*"
# mine lists devvm paste's files in $dir, one a line: regular files, never links, named from the
# characters devvm uses and no other, so a name with a newline past devvm's prefix, which would
# read as two lines, isn't devvm's.
mine() { find "$dir" -maxdepth 1 -type f -name "$pat" ! -name '*[!A-Za-z0-9._-]*'; }
case $cmd in
put)
  name=$3
  if [ -L "$dir" ]; then echo "devvm: $dir is a symbolic link, so devvm paste can't use it" >&2; exit 1; fi
  if [ ! -e "$dir" ]; then (umask 077 && mkdir -p "$dir"); fi
  if [ ! -d "$dir" ]; then echo "devvm: $dir isn't a folder, so devvm paste can't use it" >&2; exit 1; fi
  if [ -z "$(find "$dir" -prune -user "$(id -un)")" ]; then
    echo "devvm: $dir isn't yours, so devvm paste can't use it" >&2; exit 1
  fi
  umask 077
  cat > "$dir/$name.part"
  mv -f "$dir/$name.part" "$dir/$name"
  ;;
count)
  [ -d "$dir" ] || { echo 0; exit 0; }
  mine | wc -l | tr -d ' '
  ;;
rm)
  [ -d "$dir" ] || { echo 0; exit 0; }
  n=$(mine | wc -l | tr -d ' ')
  mine | while IFS= read -r f; do rm -f -- "$f"; done
  echo "$n"
  ;;
*)
  echo "devvm: paste.sh: unknown command $cmd" >&2; exit 2
  ;;
esac
