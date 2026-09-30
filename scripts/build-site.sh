#!/bin/sh
# Builds the GitHub Pages site into .cache/site from site/ (landing page, template, style)
# and the two guides in docs/ (usage.md, handoff.md), with pandoc. make site runs it in a
# pandoc container; it needs only pandoc and a POSIX shell.
#
#   SITE_REPO   the repository's URL, for the links to files that are not on the site
#               (default https://github.com/srgsf/adsvc)
set -eu

out=.cache/site
repo=${SITE_REPO:-https://github.com/srgsf/adsvc}

rm -rf "$out"
mkdir -p "$out"
cp site/style.css "$out/"
cp web/static/img/logo-256.png web/static/img/favicon-32.png "$out/"
touch "$out/.nojekyll"

page() { # page <source> <output> <extra pandoc args>...
	src=$1 dst=$2
	shift 2
	pandoc -f gfm -t html5 --standalone --template site/template.html --lua-filter site/filter.lua \
		--toc --toc-depth=2 -M repo="$repo" "$@" -o "$out/$dst" "$src"
}

page site/index.md index.html -M home=true -M description="adsvc finds known ads and intros in video streams, so that players can skip them."
page docs/usage.md usage.html -M usage=true -M pagetitle="User guide" -M description="Running adsvc: users, configuration, the HTTP API, the web page, federation and metrics."
page docs/handoff.md handoff.html -M handoff=true -M pagetitle="Client integration" -M description="How a player or app uses adsvc: the stream URL, authentication, the findings API and deployment."

echo "site: $(ls "$out" | wc -l | tr -d ' ') files in $out"
