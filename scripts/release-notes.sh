#!/usr/bin/env bash
# Prints the release notes for a version: its section of CHANGELOG.md, then how to install
# and check the release. Used by the release workflow.
#
#   scripts/release-notes.sh v1.2.3
set -euo pipefail
version="${1:?usage: scripts/release-notes.sh vX.Y.Z}"
cd "$(dirname "$0")/.."

section="$(awk -v v="$version" '
  /^## / { if (found) exit; if (index($0, "[" v "]")) { found = 1; next } }
  /^\[[^]]*\]: / { next }  # link definitions at the end of the file
  found { print }
' CHANGELOG.md)"
if [ -z "${section//[[:space:]]/}" ]; then
  echo "CHANGELOG.md has no section \"## [$version]\"" >&2
  exit 1
fi
printf '%s\n' "$section"
cat <<NOTES

## Install

On a fresh Ubuntu 24.04+ or Debian 12+ server:

\`\`\`bash
curl -fsSL https://github.com/ekPanel/ekpanel/releases/download/$version/install.sh | sudo bash
\`\`\`

An existing panel updates itself: **Settings → Updates**. See the
[installation guide](https://ekpanel.github.io/ekPanel-docs/start/install/).

## The ekctl command-line tool

For your own computer: \`ekctl-$version-<os>-<arch>.tar.gz\` (Linux, macOS and Windows, amd64 and
arm64). Unpack it, put \`ekctl\` on your PATH, make an API token in the panel (**API tokens**)
and run \`ekctl login --url https://your-panel\`.

## Check the files

\`SHA256SUMS\` is signed with the ekPanel release key
([\`internal/release/release.pub\`](https://github.com/ekPanel/ekpanel/blob/$version/internal/release/release.pub)).
The installer and the panel check it themselves. To check by hand:

\`\`\`bash
openssl pkeyutl -verify -pubin -inkey release.pub -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify ekpanel-$version-linux-amd64.tar.gz --repo ekPanel/ekpanel
\`\`\`
NOTES
