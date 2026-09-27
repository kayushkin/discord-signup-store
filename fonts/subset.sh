#!/usr/bin/env bash
# Builds static/fonts: the two web fonts cut down to the characters the pages
# use, as woff2, from Google's copies of the originals. Run it again after
# adding a character to a template that is not in UNICODES below; anything
# outside the set draws in the system font instead.
#
# Needs Python with fonttools and brotli (pip install fonttools brotli).
set -euo pipefail
cd "$(dirname "$0")/.."

# Latin, Latin-1 and Latin Extended-A; general punctuation; currency; arrows;
# geometric shapes; dingbats; and the katakana of イベント.
UNICODES="U+0020-007E,U+00A0-017F,U+2000-206F,U+20A0-20CF,U+2190-21FF,U+25A0-25FF,U+2700-27BF,U+30A4,U+30C8,U+30D9,U+30F3"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
base="https://github.com/google/fonts/raw/main/ofl"
for font in zenkakugothicnew/ZenKakuGothicNew-Regular zenkakugothicnew/ZenKakuGothicNew-Medium \
	zenkakugothicnew/ZenKakuGothicNew-Bold zenkakugothicnew/ZenKakuGothicNew-Black \
	delagothicone/DelaGothicOne-Regular; do
	name="$(basename "$font")"
	curl -sfL -o "$work/$name.ttf" "$base/$font.ttf"
	python3 -m fontTools.subset "$work/$name.ttf" --unicodes="$UNICODES" --layout-features='*' \
		--flavor=woff2 --output-file="static/fonts/$name.woff2"
done
curl -sfL -o static/fonts/OFL-ZenKakuGothicNew.txt "$base/zenkakugothicnew/OFL.txt"
curl -sfL -o static/fonts/OFL-DelaGothicOne.txt "$base/delagothicone/OFL.txt"
ls -l static/fonts
