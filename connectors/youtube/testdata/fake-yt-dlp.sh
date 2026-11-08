#!/bin/sh
# Fake yt-dlp for ytdlp_test.go, speaking the provider's two-call protocol:
#   1. --dump-single-json  -> print an info JSON listing caption tracks
#   2. --load-info-json    -> write exactly the --sub-langs track asked for
# FAKE_YTDLP_MODE selects the video: both (creator en-US + auto en-orig/en
# + a machine-translated en-de), auto (auto only), none, fail, sleep.
# Every argv is appended to $FAKE_YTDLP_ARGS; any call without
# --skip-download fails, so a regression that would fetch media fails the test.
printf '%s\n' "---" "$@" >> "$FAKE_YTDLP_ARGS"
skip=0; dump=0; load=0; dir=.; langs=""
for a in "$@"; do
  [ "$a" = "--skip-download" ] && skip=1
  [ "$a" = "--dump-single-json" ] && dump=1
  [ "$a" = "--load-info-json" ] && load=1
done
while [ $# -gt 0 ]; do case "$1" in --paths) dir="$2"; shift;; --sub-langs) langs="$2"; shift;; esac; shift; done
[ "$skip" = 1 ] || { echo "ERROR: fake refuses to download media" >&2; exit 2; }
case "$FAKE_YTDLP_MODE" in
  fail) echo "ERROR: [youtube] vid1: Sign in to confirm you're not a bot" >&2; exit 1;;
  sleep) sleep 30;;
esac
if [ "$dump" = 1 ]; then
  manual='{}'; auto='{}'
  [ "$FAKE_YTDLP_MODE" = both ] && manual='{"en-US":[{"ext":"vtt"}],"live_chat":[{"ext":"json"}]}'
  [ "$FAKE_YTDLP_MODE" = both ] || [ "$FAKE_YTDLP_MODE" = auto ] && auto='{"en-orig":[{"ext":"vtt"}],"en":[{"ext":"vtt"}],"en-de":[{"ext":"vtt"}]}'
  printf '{"id":"vid1","subtitles":%s,"automatic_captions":%s}\n' "$manual" "$auto"
  exit 0
fi
[ "$load" = 1 ] || { echo "ERROR: second call must reuse the info JSON" >&2; exit 3; }
code=$(printf '%s' "$langs" | sed -e 's/^\^//' -e 's/\$$//' -e 's/\\//g')
case "$code" in
  en-US) cat > "$dir/vid1.en-US.vtt" <<'V'
WEBVTT
Kind: captions
Language: en-US

00:00:00.000 --> 00:00:03.000
Creator line one.

00:01:05.000 --> 00:01:08.000
Creator line two &amp; more.
V
  ;;
  en-orig|en) cat > "$dir/vid1.$code.vtt" <<'V'
WEBVTT
Kind: captions
Language: en

00:00:00.000 --> 00:00:02.000 align:start position:0%
hello<00:00:00.500><c> world</c>

00:00:02.000 --> 00:00:04.000 align:start position:0%
hello world

00:00:02.000 --> 00:00:04.000 align:start position:0%
this is auto
V
  ;;
  *) echo "ERROR: unexpected track $code" >&2; exit 4;;
esac
exit 0
