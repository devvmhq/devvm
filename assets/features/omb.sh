#!/bin/bash
# oh-my-bash: themed prompt, completions, aliases
set -euxo pipefail
# Pinned commit (scripts/bump.sh updates it); git checks the content against it.
omb_commit=94c2977eda06f230aafa9628fcd869123f01896b

# Its own installer, from the pinned commit and cloning from it (OSH_REPOSITORY), so nothing comes
# from master. Replaces ~/.bashrc (backed up first); re-add the ~/.bashrc.d loader afterwards.
tmp=$(mktemp -d)
git clone -q https://github.com/ohmybash/oh-my-bash "$tmp/omb"
git -C "$tmp/omb" checkout -q -B pinned "$omb_commit"
OSH_REPOSITORY="$tmp/omb" bash "$tmp/omb/tools/install.sh" --unattended
rm -rf "$tmp"
git -C ~/.oh-my-bash rev-parse HEAD | grep -x "$omb_commit"
git -C ~/.oh-my-bash remote set-url origin https://github.com/ohmybash/oh-my-bash
grep -qs bashrc.d ~/.bashrc || echo 'for f in ~/.bashrc.d/*.sh; do [ -r "$f" ] && . "$f"; done' >> ~/.bashrc
# No background update checks; the image is rebuilt to update.
sed -i 's/^# *DISABLE_AUTO_UPDATE="true"/DISABLE_AUTO_UPDATE="true"/' ~/.bashrc
grep -q '^DISABLE_AUTO_UPDATE="true"' ~/.bashrc
